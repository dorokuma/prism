package util_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dorokuma/prism/internal/util"
)

func TestRedactBody(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{
			input:    `{"error":"invalid api key sk-FAKE_KEY_FOR_TESTING_1234567890","message":"bad key"}`,
			expected: `{"error":"invalid api key sk-***","message":"bad key"}`,
		},
		{
			input:    `{"error":"Bearer abc123def456ghi789jkl012 token invalid"}`,
			expected: `{"error":"Bearer *** token invalid"}`,
		},
		{
			input:    `{"error":"api key sk-FAKE-KEY-WITH-DASHES-FOR-TESTING with dashes"}`,
			expected: `{"error":"api key sk-*** with dashes"}`,
		},
		{
			input:    `{"error":"api key sk-FAKE_KEY_WITH_UNDERSCORES_FOR_TESTING"}`,
			expected: `{"error":"api key sk-***"}`,
		},
		{
			input:    `{"message":"no sensitive data here"}`,
			expected: `{"message":"no sensitive data here"}`,
		},
		{
			input:    `{"api_key":"sk-xxx","data":{"token":"t1","name":"ok"}}`,
			expected: `{"api_key":"***","data":{"name":"ok","token":"***"}}`,
		},
		{
			input:    `{"password":"secret123","nested":{"ACCESS_TOKEN":"tok-abc","info":"keep"}}`,
			expected: `{"nested":{"ACCESS_TOKEN":"***","info":"keep"},"password":"***"}`,
		},
		{
			input:    `{"authorization":"Bearer abc","secret":"s3cr3t"}`,
			expected: `{"authorization":"***","secret":"***"}`,
		},
	}
	for _, tc := range tests {
		got := util.RedactBody([]byte(tc.input))
		// Normalize via json.Unmarshal + json.Marshal to handle key ordering differences
		var gotMap, wantMap map[string]any
		json.Unmarshal([]byte(got), &gotMap)
		json.Unmarshal([]byte(tc.expected), &wantMap)
		gotNorm, _ := json.Marshal(gotMap)
		wantNorm, _ := json.Marshal(wantMap)
		if string(gotNorm) != string(wantNorm) {
			t.Errorf("RedactBody(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestRedactJSONTooDeep(t *testing.T) {
	// Build a JSON object with 21+ levels of nesting.
	// The innermost level should become "<redacted:too deep>".
	var build func(depth int) any
	build = func(depth int) any {
		if depth >= 22 {
			return map[string]any{"secret": "sk-leak"}
		}
		return map[string]any{"a": build(depth + 1)}
	}
	deep := build(1)
	raw, err := json.Marshal(deep)
	if err != nil {
		t.Fatal(err)
	}

	result := util.RedactBody(raw)

	// The innermost object at depth > 20 should be replaced.
	// json.Marshal escapes < and >, so look for the escaped form.
	if !strings.Contains(result, `\u003credacted:too deep\u003e`) {
		t.Fatalf("expected escaped '<redacted:too deep>' in output, got: %s", result)
	}
	// The secret key should NOT appear.
	if strings.Contains(result, "sk-leak") {
		t.Fatal("sk-leak should not appear in the redacted output")
	}

	// Verify the outer structure is still JSON.
	var parsed any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("redacted output is not valid JSON: %v", err)
	}

	// Check that RedactBodyBytes handles depth > 20 via RedactJSON.
	tooDeep := map[string]any{
		"l1": map[string]any{
			"l2": map[string]any{
				"l3": map[string]any{
					"l4": map[string]any{
						"l5": map[string]any{
							"l6": map[string]any{
								"l7": map[string]any{
									"l8": map[string]any{
										"l9": map[string]any{
											"l10": map[string]any{
												"l11": map[string]any{
													"l12": map[string]any{
														"l13": map[string]any{
															"l14": map[string]any{
																"l15": map[string]any{
																	"l16": map[string]any{
																		"l17": map[string]any{
																			"l18": map[string]any{
																				"l19": map[string]any{
																					"l20": map[string]any{
																						"l21": map[string]any{
																							"token": "abc",
																						},
																					},
																				},
																			},
																		},
																	},
																},
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	raw2, _ := json.Marshal(tooDeep)
	result2 := util.RedactBody(raw2)
	// The inner 21st level depth reaches > 20, so it should be redacted:too deep.
	if !strings.Contains(result2, `\u003credacted:too deep\u003e`) {
		t.Fatalf("nested literal: expected escaped '<redacted:too deep>' in output, got: %s", result2)
	}
}

func TestRedact_AccountKey(t *testing.T) {
	// redactBodyBytesWithKeys replaces the account key as a literal substring.
	body := []byte(`{"error":{"message":"auth failed for key abc123sekret","code":"unauthorized"}}`)
	got := util.RedactBodyBytesWithKeys(body, []string{"abc123sekret"})
	if bytes.Contains(got, []byte("abc123sekret")) {
		t.Errorf("account key not redacted: %s", got)
	}
	if !bytes.Contains(got, []byte("***")) {
		t.Error("expected *** redaction marker not found")
	}

	// sensitiveJSONKeys with key/client_key/session_key → values replaced with ***.
	body2 := []byte(`{"key":"my-secret-key","client_key":"ck-secret","session_key":"sk-secret","name":"ok"}`)
	got2 := util.RedactBodyBytes(body2)
	var m map[string]any
	if err := json.Unmarshal(got2, &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, k := range []string{"key", "client_key", "session_key"} {
		if v, ok := m[k]; !ok || v != "***" {
			t.Errorf("sensitive key %q not redacted: %v", k, v)
		}
	}
	if m["name"] != "ok" {
		t.Errorf("non-sensitive key 'name' was modified: %v", m["name"])
	}

	// sk- prefixed tokens still covered by original regex.
	body3 := []byte(`{"error":"invalid key sk-FAKE1234567890"}`)
	got3 := util.RedactBodyBytes(body3)
	if bytes.Contains(got3, []byte("sk-FAKE1234567890")) {
		t.Errorf("sk- key not redacted by regex: %s", got3)
	}
	if !bytes.Contains(got3, []byte("sk-***")) {
		t.Errorf("expected 'sk-***' redaction marker, got: %s", got3)
	}
}

func TestRedact_ExistingBehaviorUnchanged(t *testing.T) {
	// Ensure RedactBodyBytes without extraKeys behaves identically to before.
	tests := []struct {
		input    string
		expected string
	}{
		{
			input:    `{"error":"api key sk-FAKE_KEY_FOR_TESTING_1234567890","message":"bad"}`,
			expected: `{"error":"api key sk-***","message":"bad"}`,
		},
		{
			input:    `{"api_key":"sk-xxx","data":{"token":"t1","name":"ok"}}`,
			expected: `{"api_key":"***","data":{"name":"ok","token":"***"}}`,
		},
	}
	for _, tc := range tests {
		got := util.RedactBodyBytes([]byte(tc.input))
		var gotMap, wantMap map[string]any
		json.Unmarshal(got, &gotMap)
		json.Unmarshal([]byte(tc.expected), &wantMap)
		gotNorm, _ := json.Marshal(gotMap)
		wantNorm, _ := json.Marshal(wantMap)
		if string(gotNorm) != string(wantNorm) {
			t.Errorf("RedactBodyBytes(%q) = %s, want %s", tc.input, gotNorm, wantNorm)
		}
	}
}
