package agyusage

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// Generation is one de-duplicated model call extracted from an agy
// conversation database. Tokens follow GeneratorMetadata field 4:
// Prompt=f1 (fresh, excluding cache), Cached=f2, Completion=f3, Reasoning=f5.
type Generation struct {
	ConversationID   string
	RequestID        string
	TsUnix           int64
	Model            string
	PromptTokens     int64
	CachedTokens     int64
	CompletionTokens int64
	ReasoningTokens  int64
	TotalTokens      int64
}

func roDSN(path string) string {
	return "file:" + url.PathEscape(path) + "?mode=ro&_pragma=busy_timeout(5000)"
}

func dsn(path string) string {
	return "file:" + url.PathEscape(path) +
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"
}

// readConversation is the parse entry used by Refresh; tests replace it to
// count invocations (mtime skip).
var readConversation = parseConversationFile

func parseConversationFile(path string) ([]Generation, error) {
	db, err := sql.Open("sqlite", roDSN(path))
	if err != nil {
		return nil, fmt.Errorf("agyusage: open %s: %w", path, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("agyusage: ping %s: %w", path, err)
	}

	convID := strings.TrimSuffix(filepath.Base(path), ".db")
	steps := map[int64][]byte{}
	srows, err := db.QueryContext(ctx, `SELECT idx, metadata FROM steps`)
	if err != nil {
		return nil, fmt.Errorf("agyusage: steps %s: %w", path, err)
	}
	for srows.Next() {
		var idx int64
		var meta []byte
		if err := srows.Scan(&idx, &meta); err != nil {
			srows.Close()
			return nil, err
		}
		steps[idx] = meta
	}
	if err := srows.Err(); err != nil {
		srows.Close()
		return nil, err
	}
	srows.Close()

	grows, err := db.QueryContext(ctx, `SELECT idx, data FROM gen_metadata`)
	if err != nil {
		return nil, fmt.Errorf("agyusage: gen_metadata %s: %w", path, err)
	}
	defer grows.Close()

	seen := map[string]struct{}{}
	out := make([]Generation, 0, 8)
	for grows.Next() {
		var idx int64
		var data []byte
		if err := grows.Scan(&idx, &data); err != nil {
			return nil, err
		}
		g, ok := parseGeneration(convID, idx, data, steps[idx])
		if !ok {
			continue
		}
		if _, dup := seen[g.RequestID]; dup {
			continue
		}
		seen[g.RequestID] = struct{}{}
		out = append(out, g)
	}
	if err := grows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func parseGeneration(convID string, idx int64, data, stepMeta []byte) (Generation, bool) {
	if len(data) == 0 {
		return Generation{}, false
	}
	gm := messageField(data, 1)
	if gm == nil {
		return Generation{}, false
	}
	bag := messageField(gm, 4)
	if bag == nil {
		return Generation{}, false
	}
	u := parseUsageBag(bag)
	ts := parseStepUnix(stepMeta)
	if ts <= 0 {
		return Generation{}, false
	}
	req := parseRequestID(gm)
	if req == "" {
		req = fmt.Sprintf("%s-%d", convID, idx)
	}
	return Generation{
		ConversationID:   convID,
		RequestID:        req,
		TsUnix:           ts,
		Model:            parseModel(gm),
		PromptTokens:     u.Prompt,
		CachedTokens:     u.Cached,
		CompletionTokens: u.Completion,
		ReasoningTokens:  u.Reasoning,
		TotalTokens:      u.total(),
	}, true
}
