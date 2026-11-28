package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/staysup/agentbus/queue"
)

// A runner that holds no secret gives its agent the turn's stand-in as its
// model credential and the turn token for the API only. A control that sent
// no stand-in (one from before them) gets the turn refused in words, never
// started on the turn token or on this machine's own sign-in.
func TestRequestStandIn(t *testing.T) {
	r := &Runner{cfg: Config{NoSecrets: true, Queue: "http://control"}}
	job := queue.Job{Kind: queue.KindTurn, Turn: "t_1", Session: "s1", Spec: queue.Spec{Harness: "claude", Prompt: "hi"}}
	at := &attempt{r: r, job: job, token: "abr_r.a.1.sig", standIn: "abs_sealed"}
	req := r.request(context.Background(), at)
	if req.StandIn != "abs_sealed" || req.Token != "abr_r.a.1.sig" || req.APIURL != "http://control" || req.Signin != "" {
		t.Fatalf("request: stand-in %q, token %q, api %q, signin %q", req.StandIn, req.Token, req.APIURL, req.Signin)
	}
	at.standIn = ""
	if req = r.request(context.Background(), at); req.StandIn != "" || !strings.Contains(req.Signin, "no stand-in") {
		t.Fatalf("no stand-in from control: stand-in %q, signin %q", req.StandIn, req.Signin)
	}
	// Control's own word on the sign-in comes first.
	at.signin = "connect your claude subscription in Settings"
	if req = r.request(context.Background(), at); req.Signin != at.signin {
		t.Fatalf("signin %q", req.Signin)
	}
	// A runner with its own sign-in gives its agent no stand-in.
	r.cfg = Config{Forward: true, Queue: "http://control"}
	at.standIn, at.signin = "abs_sealed", ""
	if req = r.request(context.Background(), at); req.StandIn != "" || req.Token != at.token {
		t.Fatalf("own sign-in: stand-in %q, token %q", req.StandIn, req.Token)
	}
}
