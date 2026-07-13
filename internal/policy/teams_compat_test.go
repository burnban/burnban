package policy_test

import (
	"testing"

	"github.com/burnban/burnban/internal/policy"
)

// This vector is duplicated in burnban-teams/internal/policyv2. It makes a
// control-plane/agent schema drift fail both repositories before release.
func TestTeamsCanonicalPolicyCompatibilityVector(t *testing.T) {
	const input = `{
  "kind":"PolicySet",
  "apiVersion":"burnban.dev/v2",
  "metadata":{"revision":7,"namespace":"team-prod","name":"fleet"},
  "rules":[{"limits":{"tokens":[{"window_type":"rolling","window":"1m","max":1000,"id":"tpm"}]},"scope":{"user":["alice@example.com"],"project":["prod"]},"id":"principal-prod"}]
}`
	const canonical = `{"apiVersion":"burnban.dev/v2","kind":"PolicySet","metadata":{"name":"fleet","namespace":"team-prod","revision":7},"mode":"enforce","rules":[{"id":"principal-prod","scope":{"user":["alice@example.com"],"project":["prod"]},"match":{"provider":{},"model":{},"route":{},"tier":{},"geo":{}},"limits":{"tokens":[{"id":"tpm","max":1000,"window":"1m","window_type":"rolling"}]}}]}`
	const digest = "58a952f8dff6807f82d8161ca925c9b2923098b73f64ea0168cf935296976320"
	compiled, err := policy.Parse([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if string(compiled.Canonical) != canonical || compiled.Digest != digest {
		t.Fatalf("policy wire drift: canonical=%s digest=%s", compiled.Canonical, compiled.Digest)
	}
}

func TestTeamsExpandedPolicyCompatibilityVector(t *testing.T) {
	const input = `{
  "kind":"PolicySet","apiVersion":"burnban.dev/v2",
  "metadata":{"revision":8,"namespace":"expanded","name":"fleet-expanded"},"mode":"warn",
  "rules":[{"id":"all-dimensions","scope":{
    "organization":["org"],"tenant":["teams:wsp"],"meter":["mtr"],"device":["dev"],
    "team":["team"],"cost_center":["cc"],"principal":["alice"],"service_account":["ci"],
    "user":["alice"],"project":["prod"],"environment":["production"],"agent":["codex"],
    "session":["run"],"provider":["openai"],"model":["gpt-*"],"model_class":["frontier"],
    "route":["/v1/*"],"tier":["priority"],"service_tier":["priority"],"geo":["us"],"inference_geo":["us"]},
    "match":{"provider":{"allow":["openai"]},"model_class":{"deny":["unknown"]},
      "service_tier":{"allow":["priority"]},"inference_geo":{"allow":["us"]}},
    "limits":{"requests":[{"id":"rpm","max":10,"window":"1m","window_type":"rolling"}],
      "tokens":[{"id":"in","max":100,"window":"1m","window_type":"fixed","kind":"input"},
        {"id":"out","max":200,"window":"1m","window_type":"rolling","kind":"output"},
        {"id":"total","max":300,"window":"1h","window_type":"rolling","kind":"total"}],
      "dollars":[{"id":"usd","max_microusd":125000,"window":"1h","window_type":"fixed"}],
      "concurrency":2,"require_output_bound":true}}]}`
	compiled, err := policy.Parse([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	const canonical = `{"apiVersion":"burnban.dev/v2","kind":"PolicySet","metadata":{"name":"fleet-expanded","namespace":"expanded","revision":8},"mode":"warn","rules":[{"id":"all-dimensions","scope":{"organization":["org"],"tenant":["teams:wsp"],"meter":["mtr"],"device":["dev"],"team":["team"],"cost_center":["cc"],"principal":["alice"],"service_account":["ci"],"user":["alice"],"project":["prod"],"environment":["production"],"agent":["codex"],"session":["run"],"provider":["openai"],"model":["gpt-*"],"model_class":["frontier"],"route":["/v1/*"],"tier":["priority"],"service_tier":["priority"],"geo":["us"],"inference_geo":["us"]},"match":{"provider":{"allow":["openai"]},"model":{},"route":{},"tier":{},"geo":{},"model_class":{"deny":["unknown"]},"service_tier":{"allow":["priority"]},"inference_geo":{"allow":["us"]}},"limits":{"requests":[{"id":"rpm","max":10,"window":"1m","window_type":"rolling"}],"tokens":[{"id":"in","max":100,"window":"1m","window_type":"fixed","kind":"input"},{"id":"out","max":200,"window":"1m","window_type":"rolling","kind":"output"},{"id":"total","max":300,"window":"1h","window_type":"rolling","kind":"total"}],"dollars":[{"id":"usd","max_microusd":125000,"window":"1h","window_type":"fixed"}],"concurrency":2,"require_output_bound":true}}]}`
	const digest = "2c34018f9dfa8b968fa2fb81076ecadde219971a4643151a865042fc51e66270"
	if string(compiled.Canonical) != canonical || compiled.Digest != digest {
		t.Fatalf("expanded policy wire drift: canonical=%s digest=%s", compiled.Canonical, compiled.Digest)
	}
}
