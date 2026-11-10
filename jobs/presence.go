package jobs

import (
	"time"

	"github.com/staysup/agentbus/queue"
)

// Presence notes the runners that stop answering and the ones that come
// back: a runner is answering while it asked for work within
// queue.RunnerFresh. It knows only what it saw since control started, so a
// runner already quiet then is not noted.
type Presence struct {
	answering map[string]bool
}

// Check compares the runners now with the last check.
func (p *Presence) Check(q *queue.Queue, now time.Time) (gone, back []queue.Runner, err error) {
	rs, err := q.Runners(queue.DefaultTeam)
	if err != nil {
		return nil, nil, err
	}
	first := p.answering == nil
	if first {
		p.answering = map[string]bool{}
	}
	for _, r := range rs {
		ok := now.Sub(r.Seen) <= queue.RunnerFresh
		was, known := p.answering[r.ID]
		switch {
		case !known && first:
		case was && !ok:
			gone = append(gone, r)
		case !was && ok && known:
			back = append(back, r)
		}
		p.answering[r.ID] = ok
	}
	return gone, back, nil
}
