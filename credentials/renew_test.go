package credentials

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeJWT is a token shaped as an OAuth access token that expires at exp: no
// one's credential, and its signature is not one.
func fakeJWT(name string, exp time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(`{"exp":`+strconv.FormatInt(exp.Unix(), 10)+`,"n":"`+name+`"}`)) + ".fake-signature"
}

// fakeIssuer is a fake OAuth token endpoint: it answers status, and on 200 a
// new access token valid an hour and a rotated refresh token.
type fakeIssuer struct {
	*httptest.Server
	status atomic.Int32
	calls  atomic.Int32
	mu     sync.Mutex
	bodies []map[string]string
	now    func() time.Time
	// during, when set, runs while the vendor holds a renewal's request.
	during func()
}

func newFakeIssuer(t *testing.T, now func() time.Time) *fakeIssuer {
	f := &fakeIssuer{now: now}
	f.status.Store(200)
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := f.calls.Add(1)
		var b map[string]string
		if r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&b) != nil {
			w.WriteHeader(415)
			return
		}
		f.mu.Lock()
		f.bodies = append(f.bodies, b)
		f.mu.Unlock()
		// A slow vendor, so two renewals would overlap if they could.
		time.Sleep(50 * time.Millisecond)
		if f.during != nil {
			f.during()
		}
		if s := int(f.status.Load()); s != 200 {
			w.WriteHeader(s)
			// What a vendor may echo back: never passed on.
			w.Write([]byte(`{"error":"invalid_grant","refresh_token":"` + b["refresh_token"] + `"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"access_token": fakeJWT("access-"+strconv.Itoa(int(n)), f.now().Add(time.Hour)),
			"refresh_token": "fake-refresh-" + strconv.Itoa(int(n)), "id_token": "fake-id"})
	}))
	t.Cleanup(f.Close)
	return f
}

// renewing is a store of codex's shape: an OAuth sign-in kept, at a clock the
// test moves.
func renewing(t *testing.T) (*Subscriptions, *fakeIssuer, *time.Time) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	f := newFakeIssuer(t, func() time.Time { return now })
	s := &Subscriptions{Dir: t.TempDir(), Now: func() time.Time { return now },
		OAuth: map[string]OAuth{"codex": {URL: f.URL, ClientID: "fake-client", AccountHeader: "ChatGPT-Account-ID"}}}
	if err := s.Keep("codex", Signin{Token: fakeJWT("access-0", now.Add(time.Hour)), Refresh: "fake-refresh-0", Account: "acct-1",
		Expires: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return s, f, &now
}

func TestRenewWhenDue(t *testing.T) {
	s, f, now := renewing(t)
	need := Need{Access: AccessSubscription, Harness: "codex"}
	// Not due: the token as kept, with its account, and no call.
	c, err := s.Credential(need)
	if err != nil || !strings.Contains(c.Token, ".") || c.Headers["ChatGPT-Account-ID"] != "acct-1" || f.calls.Load() != 0 {
		t.Fatalf("%v %v %d", err, c.Headers, f.calls.Load())
	}
	// Four minutes left: renewed first, by the vendor's own request.
	*now = now.Add(56 * time.Minute)
	c, err = s.Credential(need)
	if err != nil || c.Token != fakeJWT("access-1", now.Add(time.Hour)) || c.Headers["ChatGPT-Account-ID"] != "acct-1" {
		t.Fatalf("%v %v", err, c.Headers)
	}
	if b := f.bodies[0]; len(b) != 3 || b["grant_type"] != "refresh_token" || b["client_id"] != "fake-client" || b["refresh_token"] != "fake-refresh-0" {
		t.Fatalf("the renewal asked %v", b)
	}
	// The rotated refresh token is kept and sent next time.
	*now = now.Add(56 * time.Minute)
	if _, err := s.Credential(need); err != nil || f.bodies[1]["refresh_token"] != "fake-refresh-1" {
		t.Fatalf("%v %v", err, f.bodies)
	}
	st, _ := s.Status("codex")
	if !st.Connected || !st.Renews || st.Expired || st.Error != "" {
		t.Fatalf("%+v", st)
	}
	fi, err := os.Stat(filepath.Join(s.Dir, "codex.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", err, fi)
	}
}

// A refresh token works once: calls that find the sign-in due at once renew
// it once, and all get the new token.
func TestRenewOnceAtATime(t *testing.T) {
	s, f, now := renewing(t)
	*now = now.Add(58 * time.Minute)
	var wg sync.WaitGroup
	got := make([]string, 8)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := s.Credential(Need{Access: AccessSubscription, Harness: "codex"})
			if err == nil {
				got[i] = c.Token
			}
		}()
	}
	wg.Wait()
	if f.calls.Load() != 1 {
		t.Fatalf("%d renewals", f.calls.Load())
	}
	for _, g := range got {
		if g != got[0] || g == "" {
			t.Fatalf("%q", got)
		}
	}
}

// Refused by the vendor: connect again, said by the relay and the status,
// and never asked again with the same refresh token. Nothing the vendor
// answered is passed on.
func TestRenewRefused(t *testing.T) {
	s, f, now := renewing(t)
	f.status.Store(400)
	*now = now.Add(58 * time.Minute)
	var m *Missing
	_, err := s.Credential(Need{Access: AccessSubscription, Harness: "codex"})
	if !errors.As(err, &m) || !strings.Contains(m.Why, "refused") || !strings.Contains(m.Why, "connect it again") || strings.Contains(m.Why, "fake-refresh") {
		t.Fatalf("%v", err)
	}
	st, _ := s.Status("codex")
	if !st.Expired || !strings.Contains(st.Error, "connect again") {
		t.Fatalf("%+v", st)
	}
	f.status.Store(200)
	*now = now.Add(time.Minute)
	if _, err := s.Credential(Need{Access: AccessSubscription, Harness: "codex"}); err == nil || f.calls.Load() != 1 {
		t.Fatalf("%v %d", err, f.calls.Load())
	}
	if len(s.RenewDue()) != 0 || f.calls.Load() != 1 {
		t.Fatal("a refused sign-in was renewed again")
	}
	// A new connect starts over.
	if err := s.Keep("codex", Signin{Token: fakeJWT("new", now.Add(time.Hour)), Refresh: "fake-refresh-new", Expires: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Status("codex"); st.Expired {
		t.Fatalf("%+v", st)
	}
}

// The vendor not answering for now: a token with time left is used, one
// whose time is up is not, and the next call tries again.
func TestRenewFailsForNow(t *testing.T) {
	s, f, now := renewing(t)
	f.status.Store(503)
	*now = now.Add(58 * time.Minute)
	c, err := s.Credential(Need{Access: AccessSubscription, Harness: "codex"})
	if err != nil || c.Token != fakeJWT("access-0", now.Add(2*time.Minute)) {
		t.Fatalf("%v", err)
	}
	*now = now.Add(3 * time.Minute)
	var m *Missing
	if _, err := s.Credential(Need{Access: AccessSubscription, Harness: "codex"}); !errors.As(err, &m) || !strings.Contains(m.Why, "just now") {
		t.Fatalf("%v", err)
	}
	// Its time is up, but it renews: not shown as expired.
	if st, _ := s.Status("codex"); st.Expired || !st.Renews {
		t.Fatalf("%+v", st)
	}
	f.status.Store(200)
	if _, err := s.Credential(Need{Access: AccessSubscription, Harness: "codex"}); err != nil || f.calls.Load() != 3 {
		t.Fatalf("%v %d", err, f.calls.Load())
	}
}

// Between turns control renews what is due: when it is about to expire, or
// eight days after the last renewal, as the vendor's CLI does.
func TestRenewDue(t *testing.T) {
	s, f, now := renewing(t)
	// A long-lived access token, so only the eight days make it due.
	if err := s.Keep("codex", Signin{Token: fakeJWT("long", now.Add(30*24*time.Hour)), Refresh: "fake-refresh-0", Expires: now.Add(30 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("claude", fakeToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(7 * 24 * time.Hour)
	if got := s.RenewDue(); len(got) != 0 || f.calls.Load() != 0 {
		t.Fatalf("%v", got)
	}
	*now = now.Add(24 * time.Hour)
	if got := s.RenewDue(); len(got) != 1 || got["codex"] != nil || f.calls.Load() != 1 {
		t.Fatalf("%v %d", got, f.calls.Load())
	}
	if got := s.RenewDue(); len(got) != 0 || f.calls.Load() != 1 {
		t.Fatalf("renewed again: %v", got)
	}
	// claude's token does not renew: expired is expired.
	if st, _ := s.Status("claude"); !st.Expired || st.Renews {
		t.Fatalf("%+v", st)
	}
}

// The person disconnects, or connects again, while a renewal waits on the
// vendor: what they did stands, and the renewal's result is dropped.
func TestRenewKeepsWhatHappenedMeanwhile(t *testing.T) {
	need := Need{Access: AccessSubscription, Harness: "codex"}
	t.Run("disconnected", func(t *testing.T) {
		s, f, now := renewing(t)
		*now = now.Add(58 * time.Minute)
		f.during = func() { s.Remove("codex") }
		var m *Missing
		if _, err := s.Credential(need); !errors.As(err, &m) || m.Why != NotConnected("codex") {
			t.Fatalf("%v", err)
		}
		if st, _ := s.Status("codex"); st.Connected {
			t.Fatalf("the renewal brought it back: %+v", st)
		}
	})
	t.Run("connected again", func(t *testing.T) {
		s, f, now := renewing(t)
		*now = now.Add(58 * time.Minute)
		other := fakeJWT("other-account", now.Add(time.Hour))
		f.during = func() {
			s.Keep("codex", Signin{Token: other, Refresh: "fake-refresh-other", Account: "acct-2", Expires: now.Add(time.Hour)})
		}
		c, err := s.Credential(need)
		if err != nil || c.Token != other || c.Headers["ChatGPT-Account-ID"] != "acct-2" {
			t.Fatalf("%v %v", err, c.Headers)
		}
		f.during = nil
		if c, err := s.Credential(need); err != nil || c.Token != other || f.calls.Load() != 1 {
			t.Fatalf("%v %d", err, f.calls.Load())
		}
	})
	t.Run("refused, then connected again", func(t *testing.T) {
		s, f, now := renewing(t)
		f.status.Store(401)
		*now = now.Add(58 * time.Minute)
		f.during = func() {
			s.Keep("codex", Signin{Token: fakeJWT("new", now.Add(time.Hour)), Refresh: "fake-refresh-new", Expires: now.Add(time.Hour)})
		}
		if _, err := s.Credential(need); err != nil {
			t.Fatalf("%v", err)
		}
		if st, _ := s.Status("codex"); st.Expired {
			t.Fatalf("the refusal marked the new sign-in: %+v", st)
		}
	})
}
