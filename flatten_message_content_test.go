package agent

// Deterministic regression guard for flattenMessageContentForUnion, the
// workaround for go-sdk v0.5.4's InputsUnion1 unmarshaller panicking on
// array-content assistant messages. The live e2e suite exercises it against
// real traffic; these hardcoded shapes make regressions in the workaround
// itself visible on every PR without spending API credits.

import (
	"encoding/json"
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

func TestFlattenMessageContentForUnion(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "array of output_text blocks flattens to concatenated string",
			in:   `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello, ","annotations":[]},{"type":"output_text","text":"world"}],"id":"msg_1","status":"completed"}`,
			want: `{"content":"Hello, world","id":"msg_1","role":"assistant","status":"completed","type":"message"}`,
		},
		{
			name: "string content passes through untouched",
			in:   `{"type":"message","role":"assistant","content":"hi","id":"msg_1"}`,
			want: `{"type":"message","role":"assistant","content":"hi","id":"msg_1"}`,
		},
		{
			name: "non-message type passes through untouched",
			in:   `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"t","arguments":"{}"}`,
			want: `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"t","arguments":"{}"}`,
		},
		{
			name: "non-output_text block bails and returns original",
			in:   `{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://example.com/x.png"}],"id":"msg_2"}`,
			want: `{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://example.com/x.png"}],"id":"msg_2"}`,
		},
		{
			name: "invalid JSON returns original bytes",
			in:   `{not json`,
			want: `{not json`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(flattenMessageContentForUnion([]byte(tc.in)))
			// Compare semantically where both sides are JSON; byte-for-byte otherwise.
			var gotAny, wantAny any
			if json.Unmarshal([]byte(got), &gotAny) == nil && json.Unmarshal([]byte(tc.want), &wantAny) == nil {
				gb, _ := json.Marshal(gotAny)
				wb, _ := json.Marshal(wantAny)
				if string(gb) != string(wb) {
					t.Fatalf("got %s, want %s", gb, wb)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// The end-to-end property the workaround exists for: an array-content
// assistant message, after flattening, decodes into InputsUnion1 without
// panicking. (Without the flatten this exact shape reflect-panics in
// go-sdk v0.5.4 — the reason every live tool round crashed.)
func TestFlattenedMessageDecodesIntoInputsUnion(t *testing.T) {
	raw := []byte(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}],"id":"msg_1","status":"completed"}`)
	flattened := flattenMessageContentForUnion(raw)
	var u components.InputsUnion1
	if err := json.Unmarshal(flattened, &u); err != nil {
		t.Fatalf("flattened message failed to decode into InputsUnion1: %v", err)
	}
}
