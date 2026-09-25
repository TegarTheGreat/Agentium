package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// TestOfficePreview writes the office scenes as pixel grids for a look
// (AGENTIUM_OFFICE_PREVIEW=file.json); skipped otherwise.
func TestOfficePreview(t *testing.T) {
	out := os.Getenv("AGENTIUM_OFFICE_PREVIEW")
	if out == "" {
		t.Skip("set AGENTIUM_OFFICE_PREVIEW to write a preview")
	}
	type grid struct {
		Name string     `json:"name"`
		W    int        `json:"w"`
		H    int        `json:"h"`
		Px   [][3]uint8 `json:"px"`
		Set  []bool     `json:"set"`
	}
	var gs []grid
	now := time.Now()
	for _, act := range []string{actIdle, actThink, actRead, actSearch, actWrite, actRun, actWeb, actPlan, actDelegate, actWait, actDone, actFail} {
		for _, frame := range []int{0, 1} {
			a := &actor{name: "Agentium", act: act, born: now.Add(-time.Hour), shirt: rgb{70, 130, 220}, hair: rgb{55, 40, 32}}
			c := leadScene(a, frame, now)
			g := grid{Name: act, W: c.w, H: c.h, Set: c.set}
			for _, p := range c.px {
				g.Px = append(g.Px, [3]uint8{p.r, p.g, p.b})
			}
			gs = append(gs, g)
			s := staffScene(&actor{name: "S", act: act, born: now.Add(-time.Hour), shirt: rgb{230, 126, 70}, hair: rgb{30, 30, 36}}, frame, now)
			g2 := grid{Name: "staff " + act, W: s.w, H: s.h, Set: s.set}
			for _, p := range s.px {
				g2.Px = append(g2.Px, [3]uint8{p.r, p.g, p.b})
			}
			gs = append(gs, g2)
		}
	}
	b, _ := json.Marshal(gs)
	os.WriteFile(out, b, 0o644)
}
