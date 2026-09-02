package planusage

import "testing"

func TestGroupByKeyDedup(t *testing.T) {
	fetchers := DefaultFetchers()
	accs := []AccountView{
		fakeAcc{name: "a1", provider: "xai", base: "https://api.x.ai/v1", key: "same"},
		fakeAcc{name: "a2", provider: "xai", base: "https://api.x.ai/v1", key: "same"},
		fakeAcc{name: "b1", provider: "xai", base: "https://api.x.ai/v1", key: "other"},
		fakeAcc{name: "ollama", provider: "ollama-cloud", base: "https://ollama.com/v1", key: "x"},
		fakeAcc{name: "Gemini", provider: "gemini", base: "https://cloudcode-pa.googleapis.com", key: "g"},
	}
	groups := GroupByKey(accs, fetchers)
	if len(groups) != 3 {
		t.Fatalf("groups = %d, want 3", len(groups))
	}
	if len(groups[0].Accounts) != 2 {
		t.Fatalf("first group accounts = %d, want 2", len(groups[0].Accounts))
	}
	if KeyFingerprint("same") == KeyFingerprint("other") {
		t.Fatal("different keys must not share fingerprint")
	}
}
