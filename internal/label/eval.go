package label

import (
	"fmt"
	"strings"

	"github.com/Kriso1337/handoffd/internal/journal"
)

type Miss struct {
	Label Label
	Entry journal.Entry
}

type Report struct {
	Labelled  int
	Unmatched int
	TP        int
	FP        int
	FN        int
	TN        int
	Filter    Confusion
	Triage    Confusion
	Misses    []Miss
}

type Confusion struct {
	TP, FP, FN, TN int
}

func (c Confusion) Precision() float64 {
	if c.TP+c.FP == 0 {
		return 0
	}
	return float64(c.TP) / float64(c.TP+c.FP)
}

func (c Confusion) Recall() float64 {
	if c.TP+c.FN == 0 {
		return 0
	}
	return float64(c.TP) / float64(c.TP+c.FN)
}

func (c Confusion) Total() int {
	return c.TP + c.FP + c.FN + c.TN
}

func (c *Confusion) add(predicted, actual bool) {
	switch {
	case predicted && actual:
		c.TP++
	case predicted && !actual:
		c.FP++
	case !predicted && actual:
		c.FN++
	default:
		c.TN++
	}
}

func Evaluate(labels []Label, entries []journal.Entry) Report {
	byID := map[string]journal.Entry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	var r Report
	for _, l := range Latest(labels) {
		r.Labelled++
		e, ok := byID[l.ID]
		if !ok {
			r.Unmatched++
			continue
		}
		predicted := journal.Reacted(e.Action)
		if e.Triage != nil {
			r.Triage.add(predicted, l.React)
		} else {
			r.Filter.add(predicted, l.React)
		}
		if predicted != l.React {
			r.Misses = append(r.Misses, Miss{Label: l, Entry: e})
		}
	}
	r.TP, r.FP, r.FN, r.TN = r.Filter.TP+r.Triage.TP, r.Filter.FP+r.Triage.FP, r.Filter.FN+r.Triage.FN, r.Filter.TN+r.Triage.TN
	return r
}

func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "labels %d (unmatched %d)\n", r.Labelled, r.Unmatched)
	total := Confusion{TP: r.TP, FP: r.FP, FN: r.FN, TN: r.TN}
	for _, row := range []struct {
		name string
		c    Confusion
	}{{"overall", total}, {"code filters", r.Filter}, {"triage", r.Triage}} {
		if row.c.Total() == 0 {
			continue
		}
		fmt.Fprintf(&b, "%-12s n=%d precision %.2f recall %.2f (tp %d fp %d fn %d tn %d)\n",
			row.name, row.c.Total(), row.c.Precision(), row.c.Recall(), row.c.TP, row.c.FP, row.c.FN, row.c.TN)
	}
	for _, m := range r.Misses {
		verdict := "missed reaction"
		if !m.Label.React {
			verdict = "needless reaction"
		}
		fmt.Fprintf(&b, "  %s %-8s %-17s %s/%s %s — %s\n",
			m.Entry.At.Local().Format("01-02 15:04"), m.Entry.Action, verdict, m.Entry.Channel, m.Entry.MsgTS, m.Entry.AuthorName, m.Entry.Text)
	}
	return b.String()
}
