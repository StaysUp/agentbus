package queue

// Review jobs. A review is a job of kind review that runs read-only in the
// reviewed job's workspace, on the runner that holds it, in a framework
// session of its own: the reviewer never sees the author's conversation. The
// control plane builds its prompt from what the reviewed job left in its
// result (its task, its changed files, the diff), and reads the verdict from
// the reviewer's reply when the review ends.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/staysup/agentbus/workspace"
)

// Verdicts.
const (
	VerdictPass    = "pass"
	VerdictFail    = "fail"
	VerdictUnknown = "unknown" // the reply had no verdict block that could be read
)

// Finding is one problem a reviewer named.
type Finding struct {
	Severity string `json:"severity"` // P1, P2 or P3
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Summary  string `json:"summary"`
}

// MaxFindings bounds the findings kept from one review.
const MaxFindings = 200

var (
	closingFence = regexp.MustCompile("^`{3,}$")
	openingFence = regexp.MustCompile("^`{3,}(json)?$")
)

// ParseVerdict reads the verdict block that ends text: a fenced block, with
// nothing after it but blank space and at most one recap line, holding
// exactly one object
// {"verdict":"pass"|"fail","findings":[{"severity":"P1"|"P2"|"P3",...}]}.
// It is found from the end, so a reply cut at its front, or a block the
// reviewer quoted earlier in its reply, cannot change it. Any P1 makes the
// verdict fail, whatever the reviewer wrote: work lands only with no P1.
// Anything else is unknown, never pass.
//
// The recap line: a reviewer's own settings can end every reply with one
// line, such as "**recap:** _the change is clean_" (claude, 2026-11-08). Only
// a line that starts with recap counts, so a conclusion in words after a
// quoted block still reads as unknown.
func ParseVerdict(text string) (string, []Finding) {
	lines := strings.Split(strings.ReplaceAll(strings.TrimRight(text, " \t\r\n"), "\r\n", "\n"), "\n")
	last := len(lines) - 1
	if last >= 1 && isRecap(lines[last]) {
		last--
		for last >= 0 && strings.TrimSpace(lines[last]) == "" {
			last--
		}
	}
	if last < 1 || !closingFence.MatchString(strings.TrimSpace(lines[last])) {
		return VerdictUnknown, nil
	}
	open := -1
	for i := last - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); strings.HasPrefix(t, "```") {
			if openingFence.MatchString(t) && len(strings.TrimSuffix(t, "json")) <= len(strings.TrimSpace(lines[last])) {
				open = i
			}
			break
		}
	}
	if open < 0 {
		return VerdictUnknown, nil
	}
	for {
		var v struct {
			Verdict  *string   `json:"verdict"`
			Findings []Finding `json:"findings"`
		}
		d := json.NewDecoder(strings.NewReader(strings.Join(lines[open+1:last], "\n")))
		d.DisallowUnknownFields()
		if d.Decode(&v) != nil || v.Verdict == nil || (*v.Verdict != VerdictPass && *v.Verdict != VerdictFail) {
			return VerdictUnknown, nil
		}
		if _, err := d.Token(); !errors.Is(err, io.EOF) {
			return VerdictUnknown, nil // anything after the one object
		}
		ok, p1 := true, false
		for _, f := range v.Findings {
			switch f.Severity {
			case "P1":
				p1 = true
			case "P2", "P3":
			default:
				ok = false
			}
			if strings.TrimSpace(f.Summary) == "" || f.Line < 0 {
				ok = false
			}
		}
		if !ok {
			return VerdictUnknown, nil
		}
		if v.Findings == nil {
			v.Findings = []Finding{}
		}
		if len(v.Findings) > MaxFindings {
			v.Findings = v.Findings[:MaxFindings]
		}
		verdict := *v.Verdict
		if p1 {
			verdict = VerdictFail
		}
		return verdict, v.Findings
	}
}

// isRecap says a line is a reviewer's one-line recap: "recap" first, after
// markdown's * and _ and spaces.
func isRecap(line string) bool {
	t := strings.ToLower(strings.TrimLeft(line, " \t*_"))
	return strings.HasPrefix(t, "recap:") || strings.HasPrefix(t, "recap ")
}

// MaxReviewPrompt bounds a review prompt, under the framework's 256 KiB.
const MaxReviewPrompt = 240 << 10

// VerdictFormat is the block every review prompt asks for.
const VerdictFormat = "```json\n" +
	`{"verdict":"pass","findings":[{"severity":"P2","file":"path/to/file","line":12,"summary":"what is wrong and why"}]}` +
	"\n```"

// ReviewPrompt is the prompt of a review of job j, which changed c. It says
// what changed and asks for a review in a fixed format. It carries no
// conclusion of the author's: with includeReply false, the author's reply
// is left out.
func ReviewPrompt(j Job, c *workspace.Changes, includeReply bool) (string, error) {
	r, err := ReadResult(j.Result)
	if len(j.Result) == 0 || err != nil || c == nil {
		return "", fmt.Errorf("queue: the job recorded no changed files")
	}
	var b strings.Builder
	b.WriteString("Review a change another agent made in this folder. You can read every file here; you cannot change any.\n\n")
	b.WriteString("## The task the author was given\n\n")
	b.WriteString(quoteBlock(j.Spec.Prompt))
	if includeReply && r.Reply != "" {
		b.WriteString("\n## The author's reply (first 500 characters)\n\n")
		b.WriteString(quoteBlock(r.Reply))
	}
	fmt.Fprintf(&b, "\n## What changed\n\n%d file(s) changed in this folder during the author's job, "+
		"against the folder's state when the job started.\n", c.Count)
	if c.Partial {
		b.WriteString("The author's turn may not have ended when this was recorded, so the list may be incomplete.\n")
	}
	b.WriteString("\n")
	for _, f := range c.Files {
		fmt.Fprintf(&b, "- %s (%s, %d bytes)\n", f.Path, f.Change, f.Size)
	}
	if c.Truncated {
		b.WriteString("- (more files changed; the list stops here)\n")
	}
	var diffs, contents strings.Builder
	left := false
	for _, f := range c.Files {
		switch {
		case f.Diff != nil:
			diffs.WriteString(fenced(*f.Diff, "diff"))
		case f.Content != nil:
			fmt.Fprintf(&contents, "\n### %s\n\n%s", f.Path, fenced(*f.Content, ""))
		case f.DiffLeftOut:
			left = true
		}
	}
	if diffs.Len() > 0 {
		b.WriteString("\n## The diff of each file\n\n")
		b.WriteString(diffs.String())
		if left {
			b.WriteString("(some files have no diff here: binary, too big, or their earlier content is not kept; read them)\n")
		}
	}
	if contents.Len() > 0 {
		b.WriteString("\n## Small files with no diff here, as they are now\n")
		b.WriteString(contents.String())
	}
	tail := "\n## What to do\n\nReview this change. Read the files you need. Report each problem with a severity: " +
		"P1 must be fixed before the change lands, P2 should be fixed, P3 is minor.\n\n" +
		"End your reply with exactly one fenced json block in this format, and nothing after it. " +
		"verdict is pass or fail; findings may be empty.\n\n" + VerdictFormat + "\n"
	out := b.String()
	if len(out)+len(tail) > MaxReviewPrompt {
		out = strings.ToValidUTF8(out[:MaxReviewPrompt-len(tail)-100], "") + "\n\n(the description is cut here; read the files for the rest)\n"
	}
	return out + tail, nil
}

// quoteBlock puts text in a fenced block.
func quoteBlock(s string) string { return fenced(s, "") }

// fenced puts s in a fenced block whose fence is longer than any run of
// backticks in s, so no line of s can close it and read as the prompt's own
// text. info names the block's language.
func fenced(s, info string) string {
	run, longest := 0, 0
	for _, c := range s {
		if c == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	f := strings.Repeat("`", max(3, longest+1))
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return f + info + "\n" + s + f + "\n"
}
