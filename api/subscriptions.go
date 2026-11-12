package api

// The person's own subscriptions, for runners on their own machines: each
// harness's sign-in, connected once on control with the vendor's own login
// (package credentials), so control's gateway adds it to the model calls of
// agents on those runners, which hold none. Every runner is the person's
// own: a machine joins only when they approve its code. The main token only: a turn token never
// sees or starts these. No reply ever holds a token.

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/staysup/agentbus/credentials"
)

// SubscriptionList is GET /api/v0/subscriptions.
type SubscriptionList struct {
	Subscriptions []SubscriptionState `json:"subscriptions" doc:"One per harness whose subscription can be connected here."`
}

// SubscriptionState is one harness's subscription, and its connect if one
// was started since control started.
type SubscriptionState struct {
	Harness     string            `json:"harness" doc:"The harness, such as claude."`
	Connected   bool              `json:"connected" doc:"A token is kept for it on control."`
	ConnectedAt *time.Time        `json:"connected_at,omitempty" doc:"When it was connected."`
	ExpiresAt   *time.Time        `json:"expires_at,omitempty" doc:"When the token stops working, as the vendor says. Without renews, connect again before then."`
	Renews      bool              `json:"renews" doc:"Control renews the token itself before it expires, with what the login gave it (codex's ChatGPT sign-in), or runs the harness to renew its own (agy's Google sign-in)."`
	Expired     bool              `json:"expired" doc:"The sign-in cannot be used: its time is up, or the vendor refused to renew it. Connect again."`
	Error       string            `json:"error,omitempty" doc:"Why it cannot be used, when it cannot."`
	Connect     *credentials.View `json:"connect,omitempty" doc:"The latest connect, while control runs. Its links and one-time code are shown only while it runs."`
}

// ConnectInput is the body of POST /api/v0/subscriptions/{id}/connect/input.
type ConnectInput struct {
	Line string `json:"line" doc:"One line for the login, such as the code the browser showed. It is never kept or shown."`
}

const noSubscriptions = "this service connects no subscriptions: it is not control"

var subscriptionRoutes = []route{
	{method: "GET", path: "/api/v0/subscriptions", op: "listSubscriptions",
		tag:     "Settings",
		summary: "List subscriptions",
		doc: "The person's own subscriptions that control can add to the model calls of agents on their runners, which hold no sign-in: " +
			"for each harness, whether it is connected and until when. A connected one is used for every turn, on one machine (agentbus up) as on every approved runner; a turn whose harness is not connected is refused with \"connect your <harness> subscription in Settings\".",
		replies: []reply{ok(200, SubscriptionList{}, "The subscriptions."), fail(404, noSubscriptions)},
		serve:   plain((*API).listSubscriptions)},
	{method: "POST", path: "/api/v0/subscriptions/{id}/connect", op: "connectSubscription",
		idDoc:   "A harness, such as claude. Another shape gets 404.",
		tag:     "Settings",
		summary: "Connect a subscription",
		doc: "Start the harness's own login on control (for claude: claude setup-token; for codex: codex login --device-auth; for agy: agy acp --login). " +
			"Of what it prints, only the links come back, and a one-time code to enter at the link. For claude: open the link, sign in, and " +
			"send the code the browser shows with POST .../connect/input; agy the same. For codex: open the link, sign in, and enter the code; the login " +
			"ends by itself. The sign-in is kept in control's data folder, readable by its owner only, and never shown; codex's is renewed " +
			"by control before it expires, and agy's by agy, which control runs. A login already running is answered as it is. No body is read.",
		replies: []reply{ok(202, credentials.View{}, "The login, running."), badRequest, fail(404, "No login for this harness here."),
			fail(500, "The login could not be started.")},
		serve: (*API).connectSubscription},
	{method: "GET", path: "/api/v0/subscriptions/{id}/connect", op: "getConnect",
		idDoc:   "A harness, such as claude. Another shape gets 404.",
		tag:     "Settings",
		summary: "Follow a connect",
		doc:     "The harness's latest login: running, connected or failed. While it runs, the links and one-time code it printed; nothing else it prints is shown. Once it ended, only its state and why it failed.",
		replies: []reply{ok(200, credentials.View{}, "The login."), fail(404, "No login was started for it since control started.")},
		serve:   (*API).getConnect},
	{method: "POST", path: "/api/v0/subscriptions/{id}/connect/input", op: "connectInput",
		idDoc:   "A harness, such as claude. Another shape gets 404.",
		tag:     "Settings",
		summary: "Answer a login",
		doc:     "Send one line to the running login, such as the code the browser showed after signing in. A login still running 30 seconds after it is stopped: the connect fails with rejected set, as when the login says it did not take the code.",
		body:    ConnectInput{},
		replies: []reply{{status: 204, doc: "Sent.", empty: true}, badRequest, fail(409, "No login is running for it.")},
		serve:   (*API).connectInput},
	{method: "DELETE", path: "/api/v0/subscriptions/{id}/connect", op: "cancelConnect",
		idDoc:   "A harness, such as claude. Another shape gets 404.",
		tag:     "Settings",
		summary: "Cancel a connect",
		doc:     "Stop the harness's running login. The connect then fails, with cancelled set; a new POST .../connect starts again. A token kept from an earlier connect stays. No body is read.",
		replies: []reply{{status: 204, doc: "Stopped.", empty: true}, badRequest, fail(404, noSubscriptions), fail(409, "No login is running for it.")},
		serve:   (*API).cancelConnect},
	{method: "DELETE", path: "/api/v0/subscriptions/{id}", op: "disconnectSubscription",
		idDoc:   "A harness, such as claude. Another shape gets 404.",
		tag:     "Settings",
		summary: "Disconnect a subscription",
		doc:     "Forget the harness's token on control. Runners' calls on it are refused until it is connected again. No body is read.",
		replies: []reply{{status: 204, doc: "Removed.", empty: true}, badRequest, fail(404, noSubscriptions), storeFailed},
		serve:   (*API).disconnectSubscription},
}

func (a *API) listSubscriptions(w http.ResponseWriter, r *http.Request) {
	m := a.cfg.Subscriptions
	if m == nil {
		writeError(w, http.StatusNotFound, noSubscriptions)
		return
	}
	out := SubscriptionList{Subscriptions: []SubscriptionState{}}
	names := make([]string, 0, len(m.Logins))
	for h := range m.Logins {
		names = append(names, h)
	}
	sort.Strings(names)
	for _, h := range names {
		st, err := m.Store.Status(h)
		if err != nil {
			st = credentials.Status{Harness: h}
		}
		s := SubscriptionState{Harness: h, Connected: st.Connected, ConnectedAt: st.ConnectedAt, ExpiresAt: st.ExpiresAt,
			Renews: st.Renews, Expired: st.Expired, Error: st.Error}
		if v, ok := m.View(h); ok {
			s.Connect = &v
		}
		out.Subscriptions = append(out.Subscriptions, s)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) connectSubscription(w http.ResponseWriter, r *http.Request, id string) {
	if !noBody(w, r) {
		return
	}
	if a.cfg.Subscriptions == nil {
		writeError(w, http.StatusNotFound, noSubscriptions)
		return
	}
	v, err := a.cfg.Subscriptions.Connect(id)
	var missing *credentials.Missing
	switch {
	case errors.Is(err, credentials.ErrNoLogin):
		writeError(w, http.StatusNotFound, "control has no login for "+id+"'s subscription")
	case errors.As(err, &missing):
		// Fixed wording: the program's name and control's PATH.
		writeError(w, http.StatusInternalServerError, missing.Why)
	case err != nil:
		writeError(w, http.StatusInternalServerError, "the login could not be started: is the harness's program on control's PATH?")
	default:
		writeJSON(w, http.StatusAccepted, v)
	}
}

func (a *API) getConnect(w http.ResponseWriter, r *http.Request, id string) {
	if a.cfg.Subscriptions == nil {
		writeError(w, http.StatusNotFound, noSubscriptions)
		return
	}
	v, ok := a.cfg.Subscriptions.View(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no login was started for "+id+" since control started")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) connectInput(w http.ResponseWriter, r *http.Request, id string) {
	if a.cfg.Subscriptions == nil {
		writeError(w, http.StatusNotFound, noSubscriptions)
		return
	}
	var in ConnectInput
	if !decodeObject(r, &in) || len(in.Line) > 4096 {
		writeError(w, http.StatusBadRequest, "the body must be one JSON object with line, at most 4 KiB")
		return
	}
	if err := a.cfg.Subscriptions.Input(id, in.Line); err != nil {
		writeError(w, http.StatusConflict, "no login is running for "+id)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) cancelConnect(w http.ResponseWriter, r *http.Request, id string) {
	if !noBody(w, r) {
		return
	}
	if a.cfg.Subscriptions == nil {
		writeError(w, http.StatusNotFound, noSubscriptions)
		return
	}
	if err := a.cfg.Subscriptions.Cancel(id); err != nil {
		writeError(w, http.StatusConflict, "no login is running for "+id)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) disconnectSubscription(w http.ResponseWriter, r *http.Request, id string) {
	if !noBody(w, r) {
		return
	}
	if a.cfg.Subscriptions == nil {
		writeError(w, http.StatusNotFound, noSubscriptions)
		return
	}
	if err := a.cfg.Subscriptions.Store.Remove(id); err != nil {
		writeError(w, http.StatusInternalServerError, "the subscription could not be removed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
