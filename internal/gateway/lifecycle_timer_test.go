// Controller stop against the deadline timer.

package gateway

import (
	"sync"
	"testing"
	"time"
)

// Stop ends the deadline timer: a provisional base whose deadline passes
// after the controller stopped is not rolled back by it, and the next owner
// rolls it back at startup, once.
func TestStopEndsDeadlineTimer(t *testing.T) {
	e := newEnv(t)
	provisional := e.policy(e.a, true)
	deadline := time.UnixMilli(time.Now().Add(1500 * time.Millisecond).UnixMilli())
	seed(t, e.database, e.policy(e.a, false), seedOptions{provisional: &provisional, deadline: deadline})

	c, _ := e.own(e.controllerConfig())
	if g := c.Current(); g == nil || g.Revision != 2 || !g.NotAfter.Equal(deadline) {
		t.Fatalf("published %+v, want provisional revision 2 until %s", g, deadline)
	}
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	if c.timer == nil || c.timer.Stop() {
		t.Fatal("the deadline timer was still armed after Stop")
	}
	time.Sleep(time.Until(deadline.Add(700 * time.Millisecond)))
	if n := queryInt(t, e.database, `SELECT coalesce(provisional, 0) FROM base`); n != 2 {
		t.Fatalf("provisional %d after the deadline passed while stopped, want 2", n)
	}
	if n := queryInt(t, e.database, `SELECT count(*) FROM transition WHERE id LIKE 'auto-rollback:%'`); n != 0 {
		t.Fatalf("%d rollbacks recorded by a stopped controller", n)
	}

	again, _ := e.own(e.controllerConfig())
	if g := again.Current(); g == nil || g.Revision != 1 || !g.NotAfter.IsZero() {
		t.Fatalf("after restart %+v, want confirmed revision 1", g)
	}
	if err := again.Stop(); err != nil {
		t.Fatal(err)
	}
	if n := queryInt(t, e.database, `SELECT count(*) FROM transition WHERE id = 'auto-rollback:2' AND outcome = 'committed' AND audited_at IS NOT NULL`); n != 1 {
		t.Fatalf("%d audited rollbacks, want 1", n)
	}
}

// Stop racing the deadline timer leaves the rollback either complete,
// published and audited, or not started; never half done. The next owner
// always ends with the rollback applied exactly once.
func TestStopRacesDeadline(t *testing.T) {
	outcomes := map[string]int{}
	for round := range 10 {
		e := newEnv(t)
		provisional := e.policy(e.a, true)
		deadline := time.UnixMilli(time.Now().Add(400 * time.Millisecond).UnixMilli())
		seed(t, e.database, e.policy(e.a, false), seedOptions{provisional: &provisional, deadline: deadline})
		c, _ := e.own(e.controllerConfig())

		// Stop lands from a few milliseconds before to a few after the timer.
		time.Sleep(time.Until(deadline.Add(time.Duration(round-5) * time.Millisecond)))
		var wg sync.WaitGroup
		errs := make([]error, 3)
		for i := range errs {
			wg.Go(func() { errs[i] = c.Stop() })
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatalf("round %d: Stop: %v", round, err)
			}
		}

		rolledBack := queryInt(t, e.database, `SELECT count(*) FROM transition WHERE id = 'auto-rollback:2' AND outcome = 'committed'`)
		base := queryInt(t, e.database, `SELECT coalesce(provisional, 0) FROM base`)
		unaudited := queryInt(t, e.database, `SELECT count(*) FROM transition WHERE outcome = 'committed' AND audited_at IS NULL`)
		switch {
		case rolledBack == 1 && base == 0 && unaudited == 0:
			outcomes["rolled back before the stop"]++
		case rolledBack == 0 && base == 2:
			outcomes["stopped before the rollback"]++
		default:
			t.Fatalf("round %d: rollback %d, provisional %d, unaudited %d", round, rolledBack, base, unaudited)
		}

		again, _ := e.own(e.controllerConfig())
		if g := again.Current(); g == nil || g.Revision != 1 {
			t.Fatalf("round %d: after restart %+v", round, g)
		}
		if err := again.Stop(); err != nil {
			t.Fatal(err)
		}
		if n := queryInt(t, e.database, `SELECT count(*) FROM transition WHERE id = 'auto-rollback:2'`); n != 1 {
			t.Fatalf("round %d: %d rollbacks", round, n)
		}
	}
	t.Logf("outcomes: %v", outcomes)
}
