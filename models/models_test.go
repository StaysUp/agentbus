package models

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A fake key: no one's credential.
const fakeKey = "fake-list-models-key-0000111122223333"

// fakeVendor answers each path from replies, and keeps each request's path,
// query and the headers that carry a key.
type fakeVendor struct {
	*httptest.Server
	seen []string
}

func newFake(t *testing.T, replies map[string]string) *fakeVendor {
	f := &fakeVendor{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.seen = append(f.seen, r.URL.Path+"?"+r.URL.RawQuery+" auth="+r.Header.Get("Authorization"))
		b, ok := replies[r.URL.Path+"?"+r.URL.RawQuery]
		if !ok {
			b, ok = replies[r.URL.Path]
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(b, "status:") {
			code := map[string]int{"status:401": 401, "status:500": 500}[b]
			w.WriteHeader(code)
			w.Write([]byte(`{"error":{"message":"the key ` + fakeKey + ` is wrong"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(b))
	}))
	t.Cleanup(f.Close)
	return f
}

func ids(ms []Model) string {
	var s []string
	for _, m := range ms {
		if m.Label != "" {
			s = append(s, m.ID+"="+m.Label)
		} else {
			s = append(s, m.ID)
		}
	}
	return strings.Join(s, ",")
}

func TestOpenRouterWithAndWithoutKey(t *testing.T) {
	f := newFake(t, map[string]string{"/api/v1/models": `{"data":[{"id":"deepseek/deepseek-v4.1-flash","name":"DeepSeek: V4.1 Flash"}]}`})
	for _, key := range []string{fakeKey, ""} {
		ms, err := OpenRouter(context.Background(), f.Client(), f.URL+"/api", key)
		if err != nil || ids(ms) != "deepseek/deepseek-v4.1-flash=DeepSeek: V4.1 Flash" {
			t.Fatalf("%s %v", ids(ms), err)
		}
	}
	if !strings.Contains(f.seen[0], "auth=Bearer "+fakeKey) || !strings.HasSuffix(f.seen[1], "auth=") {
		t.Fatalf("headers: %v", f.seen)
	}
}

// A refused key, a vendor that fails, and a reply that is not the list give
// fixed words: never the vendor's body, which can quote the key.
func TestErrorsAreFixedWords(t *testing.T) {
	for reply, want := range map[string]error{"status:401": ErrRefused, "status:500": ErrUnreachable, "not json": ErrUnreadable} {
		f := newFake(t, map[string]string{"/api/v1/models": reply})
		_, err := OpenRouter(context.Background(), f.Client(), f.URL+"/api", fakeKey)
		if !errors.Is(err, want) || strings.Contains(err.Error(), fakeKey) {
			t.Fatalf("%s: %v", reply, err)
		}
	}
	if _, err := OpenRouter(context.Background(), http.DefaultClient, "http://127.0.0.1:1", fakeKey); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("unreachable: %v", err)
	}
}

// Only "id<TAB>label" lines are read from a harness's own listing.
func TestLines(t *testing.T) {
	out := "Signed in as someone@example.com\n\ngemini-3.8-flash-medium\tGemini 3.8 Flash (Medium)\r\ngemini-4-argon-medium\tGemini 4 Argon\n" +
		"no tab here\n\tno id\nbad id\tx\ngemini-4-argon-medium\tagain\n"
	if got := ids(Lines([]byte(out))); got != "gemini-3.8-flash-medium=Gemini 3.8 Flash (Medium),gemini-4-argon-medium=Gemini 4 Argon" {
		t.Fatalf("%s", got)
	}
}

func TestCacheFetchesHourlyOrOnDemand(t *testing.T) {
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	c := &Cache{Now: func() time.Time { return now }}
	calls := 0
	var fail error
	fetch := func() ([]Model, error) {
		calls++
		if fail != nil {
			return nil, fail
		}
		return []Model{{ID: "m" + string(rune('0'+calls))}}, nil
	}
	l := c.Get("api/claude", FromAPI, false, fetch)
	if calls != 1 || ids(l.Models) != "m1" || l.FetchedAt == nil || !l.FetchedAt.Equal(now) || l.Source != FromAPI {
		t.Fatalf("first: %+v", l)
	}
	now = now.Add(59 * time.Minute)
	if l = c.Get("api/claude", FromAPI, false, fetch); calls != 1 || ids(l.Models) != "m1" {
		t.Fatalf("within the hour: %d %+v", calls, l)
	}
	if l = c.Get("api/claude", FromAPI, true, fetch); calls != 2 || ids(l.Models) != "m2" {
		t.Fatalf("on demand: %d %+v", calls, l)
	}
	now = now.Add(MaxAge)
	fail = ErrUnreachable
	if l = c.Get("api/claude", FromAPI, false, fetch); calls != 3 || ids(l.Models) != "m2" || l.Error != ErrUnreachable.Error() {
		t.Fatalf("a failed fetch keeps the list: %+v", l)
	}
	c.Forget("api/")
	if l = c.Get("api/claude", FromAPI, false, fetch); calls != 4 || len(l.Models) != 0 || l.FetchedAt != nil || l.Error == "" {
		t.Fatalf("forgotten, failing: %+v", l)
	}
	fail = nil
	if l = c.Get("api/claude", FromAPI, false, fetch); calls != 5 || ids(l.Models) != "m5" {
		t.Fatalf("a failed first fetch is tried again: %+v", l)
	}
}
