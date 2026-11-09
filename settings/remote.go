package settings

// Remote is the store of a runner on another machine: the server's
// settings, read through the runner protocol (GET
// /api/v0/runners/{id}/settings) with the runner's own token, fresh for each
// turn. So a policy changed on the server, by the API or by hand, holds for
// the next turn on every runner, and a runner's own files are never read.
//
// A server that cannot be reached, or whose settings cannot be used, refuses
// the turn with that reason: no turn runs on a policy older than the
// server's, since a forbid rule just added must hold.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// RunnerSettings is the reply of GET /api/v0/runners/{id}/settings: what a runner
// needs to start a turn.
type RunnerSettings struct {
	Permissions         Policy  `json:"permissions" doc:"The policy, every value filled. Folders are as written: the runner reads ~ as its own home."`
	PermissionsVersion  Version `json:"permissions_version" doc:"The policy's version, as GET /api/v0/settings/permissions sends it in ETag."`
	Instructions        string  `json:"instructions" doc:"The text every turn gets, before {session} is filled in."`
	InstructionsVersion Version `json:"instructions_version" doc:"The instructions' version."`
}

// Remote reads the server's settings.
type Remote struct {
	// Server is the server's URL, such as http://192.168.1.10:8787.
	Server string
	// Local is the runner's own data folder: its files are still the ones a
	// turn may not edit.
	Local string
	// HTTP makes the calls. Nil is a client with a 10-second timeout.
	HTTP *http.Client

	mu            sync.Mutex
	runner, token string
}

// What a turn's refusal says when the server's settings cannot be had.
const (
	remoteWhere = "the server's settings"
	remoteFix   = "Check that the server is up and this runner can reach it, or fix the settings on the server"
	folderWhere = "the server's permissions, on this runner,"
	folderFix   = "Make the folder on this runner, or change read_folders on the server"
)

// Enrolled gives the store the runner's id and token, once it has them.
func (r *Remote) Enrolled(id, token string) {
	r.mu.Lock()
	r.runner, r.token = id, token
	r.mu.Unlock()
}

// Protected are the runner's own data folder files.
func (r *Remote) Protected() []string { return protected(r.Local) }

// Policy is the server's policy.
func (r *Remote) Policy() (Policy, Version, error) {
	s, err := r.fetch("permissions")
	if err != nil {
		return Policy{}, "", err
	}
	p := s.Permissions
	if err := p.Check(); err != nil {
		return Policy{}, "", &Error{Setting: "permissions", Where: remoteWhere, Why: "the server sent a policy this runner refuses: " + err.Error(), Fix: remoteFix}
	}
	p.fill()
	p.where, p.fix = folderWhere, folderFix
	return p, s.PermissionsVersion, nil
}

// Instructions are the server's instructions.
func (r *Remote) Instructions() (string, Version, error) {
	s, err := r.fetch("instructions")
	if err != nil {
		return "", "", err
	}
	if err := CheckInstructions([]byte(s.Instructions)); err != nil {
		return "", "", &Error{Setting: "instructions", Where: remoteWhere, Why: "the server sent instructions this runner refuses: " + err.Error(), Fix: remoteFix}
	}
	return s.Instructions, s.InstructionsVersion, nil
}

// SetPolicy is refused: a runner's settings are the server's.
func (r *Remote) SetPolicy(Policy, Version) (Policy, Version, error) {
	return Policy{}, "", ErrReadOnly
}

// SetInstructions is refused: a runner's settings are the server's.
func (r *Remote) SetInstructions(string, Version) (Version, error) { return "", ErrReadOnly }

// fetch reads the snapshot. Its error is an *Error for setting.
func (r *Remote) fetch(setting string) (RunnerSettings, error) {
	fail := func(why string) (RunnerSettings, error) {
		return RunnerSettings{}, &Error{Setting: setting, Where: remoteWhere, Why: why, Fix: remoteFix}
	}
	r.mu.Lock()
	id, token := r.runner, r.token
	r.mu.Unlock()
	if id == "" {
		return fail("this runner has not enrolled with the server yet")
	}
	c := r.HTTP
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.Server+"/api/v0/runners/"+id+"/settings", nil)
	if err != nil {
		return fail("the server's URL is not one this runner can call")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.Do(req)
	if err != nil {
		return fail("the server could not be reached")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fail("the server's reply could not be read")
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return fail("the server does not serve settings to runners (HTTP 404); run the same version of agentbus on the server")
	case http.StatusUnauthorized:
		return fail("the server refused this runner's token (HTTP 401)")
	default:
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return fail("the server says: " + e.Error)
		}
		return fail("the server answered HTTP " + http.StatusText(resp.StatusCode))
	}
	var s RunnerSettings
	if err := json.Unmarshal(b, &s); err != nil {
		return fail("the server's reply is not settings")
	}
	return s, nil
}
