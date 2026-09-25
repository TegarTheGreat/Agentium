package main

import "time"

// The office's pixel art: Agentium's corner of the room (a window whose
// sky follows the clock, a poster, a desk with a glowing monitor, a
// plant) and a smaller desk for each staff member. Sprites are strings
// drawn with a palette: '.' is left alone.

const (
	sceneW, sceneH = 33, 16 // Agentium's scene, in pixels (8 rows)
	staffW, staffH = 16, 12 // a staff member's (6 rows)
)

var (
	colOutline  = rgb{32, 28, 40}
	colSkinA    = rgb{248, 208, 170}
	colSkinB    = rgb{222, 170, 132}
	colWall     = rgb{52, 57, 84}
	colWallTop  = rgb{44, 48, 72}
	colRail     = rgb{74, 79, 110}
	colWainscot = rgb{60, 62, 92}
	colBase     = rgb{32, 34, 50}
	colFloorA   = rgb{122, 86, 60}
	colFloorB   = rgb{104, 72, 50}
	colFloorC   = rgb{92, 64, 44}
	colDeskTop  = rgb{190, 136, 88}
	colDeskHi   = rgb{214, 162, 110}
	colDeskFace = rgb{142, 98, 60}
	colDeskLeg  = rgb{108, 74, 46}
	colBezel    = rgb{40, 44, 58}
	colBezelHi  = rgb{70, 76, 100}
	colWinFrame = rgb{226, 220, 206}
	colLeafA    = rgb{96, 184, 102}
	colLeafB    = rgb{60, 134, 80}
	colPotA     = rgb{200, 112, 74}
	colPotB     = rgb{158, 84, 56}
	colPants    = rgb{58, 64, 94}
	colShoe     = rgb{40, 36, 44}
	colChairA   = rgb{66, 68, 92}
	colChairB   = rgb{92, 96, 124}
	colKeyboard = rgb{66, 70, 90}
	colKeyCap   = rgb{156, 162, 184}
	colMug      = rgb{236, 236, 240}
	colSteam    = rgb{190, 196, 214}
	colPoster   = rgb{112, 86, 206}
	colPosterHi = rgb{250, 206, 104}
)

// sprite draws rows of a picture: each character is a palette entry.
func (c *canvas) sprite(x, y int, pal map[rune]rgb, rows ...string) {
	for j, r := range rows {
		i := 0
		for _, ch := range r {
			if col, ok := pal[ch]; ok {
				c.dot(x+i, y+j, col)
			}
			i++
		}
	}
}

func shade(c rgb, f float64) rgb {
	sc := func(v uint8) uint8 {
		x := float64(v) * f
		if x > 255 {
			x = 255
		}
		return uint8(x)
	}
	return rgb{sc(c.r), sc(c.g), sc(c.b)}
}

// person is the palette of one character.
func person(a *actor) map[rune]rgb {
	return map[rune]rgb{
		'o': colOutline, 's': colSkinA, 'S': colSkinB, 'e': colEye,
		'h': a.hair, 'H': shade(a.hair, 1.6), 'd': shade(a.hair, 0.7),
		't': a.shirt, 'T': shade(a.shirt, 0.72), 'L': shade(a.shirt, 1.18),
		'p': colPants, 'P': shade(colPants, 0.75), 'k': colShoe,
		'c': colChairA, 'C': colChairB,
	}
}

// sky is the window's view: day, dusk or night by the local clock.
func sky(c *canvas, x, y, w, h int, now time.Time, frame int) {
	hr := now.Hour()
	switch {
	case hr >= 7 && hr < 17:
		for j := 0; j < h; j++ {
			c.rect(x, y+j, w, 1, shade(rgb{120, 186, 240}, 1+0.05*float64(j)))
		}
		cx := x + (frame/40)%(w+3) - 2 // a cloud drifts by
		c.sprite(cx, y+1, map[rune]rgb{'#': {250, 252, 255}, '+': {220, 232, 246}}, ".##.", "####", "++++")
		c.dot(x+w-2, y+1, rgb{255, 232, 140})
	case hr >= 17 && hr < 20 || hr >= 5 && hr < 7:
		cols := []rgb{{250, 176, 110}, {246, 146, 110}, {214, 118, 140}, {150, 96, 150}, {96, 76, 140}}
		for j := 0; j < h; j++ {
			c.rect(x, y+j, w, 1, cols[min(j, len(cols)-1)])
		}
		c.rect(x+w/2-1, y+h-2, 3, 2, rgb{255, 214, 130})
	default:
		c.rect(x, y, w, h, rgb{26, 34, 76})
		c.rect(x, y+h-1, w, 1, rgb{36, 46, 96})
		c.sprite(x+w-3, y+1, map[rune]rgb{'#': {246, 240, 206}}, ".#", "##", ".#")
		for i, s := range [][2]int{{1, 1}, {3, 3}, {2, 0}} {
			if (frame/6+i)%5 != 0 { // stars twinkle
				c.dot(x+s[0], y+s[1], rgb{250, 244, 200})
			}
		}
	}
}

// leadScene draws Agentium's corner of the office.
func leadScene(a *actor, frame int, now time.Time) *canvas {
	c := newCanvas(sceneW, sceneH)
	// The room.
	c.rect(0, 0, sceneW, 11, colWall)
	c.rect(0, 0, sceneW, 1, colWallTop)
	c.rect(0, 7, sceneW, 1, colRail)
	c.rect(0, 8, sceneW, 3, colWainscot)
	c.rect(0, 11, sceneW, 1, colBase)
	c.rect(0, 12, sceneW, 4, colFloorA)
	for i := 0; i < sceneW; i += 6 {
		c.dot(i+2, 13, colFloorB)
		c.dot(i+5, 14, colFloorB)
	}
	c.rect(0, 15, sceneW, 1, colFloorC)
	// Window.
	c.rect(11, 1, 7, 6, colWinFrame)
	sky(c, 12, 2, 5, 4, now, frame)
	c.rect(14, 2, 1, 4, colWinFrame)
	c.rect(11, 6, 7, 1, shade(colWinFrame, 0.8))

	drawPerson(c, 0, 0, a, frame, now) // behind the desk

	// Desk.
	c.rect(11, 9, 22, 1, colDeskHi)
	c.rect(11, 10, 22, 1, colDeskFace)
	c.rect(12, 11, 1, 4, colDeskLeg)
	c.rect(31, 11, 1, 4, colDeskLeg)
	// Monitor.
	c.rect(19, 2, 10, 6, colBezel)
	c.rect(19, 2, 10, 1, colBezelHi)
	drawMonitor(c, 20, 3, 8, 4, a.act, frame)
	c.rect(23, 8, 2, 1, colBezel)
	c.rect(21, 8, 6, 1, colBezelHi)
	// Plant.
	c.sprite(29, 3, map[rune]rgb{'l': colLeafA, 'L': colLeafB, 'p': colPotA, 'P': colPotB},
		".l.l", "lLlL", ".lLl", "..L.", ".ppp", ".pPp")
	// Keyboard and mug.
	c.rect(12, 8, 5, 1, colKeyboard)
	c.dot(13, 8, colKeyCap)
	c.dot(15, 8, colKeyCap)
	c.sprite(17, 7, map[rune]rgb{'m': colMug, 'M': shade(colMug, 0.75)}, "mM", "MM")
	if a.act == actIdle || a.act == actThink {
		c.dot(17+(frame/3)%2, 5+frame%2, colSteam)
	}

	drawArms(c, 0, 0, a, frame, now) // in front of it
	return c
}

// offset is where a new staff member is while walking in.
func walkIn(a *actor, now time.Time) int {
	if age := now.Sub(a.born); age < time.Second {
		return -int((time.Second - age) / (120 * time.Millisecond))
	}
	return 0
}

// drawPerson draws someone in a chair, facing the desk on their right.
func drawPerson(c *canvas, x, y int, a *actor, frame int, now time.Time) {
	pal := person(a)
	x += walkIn(a, now)
	c.sprite(x+1, y+5, pal, "cc", "cC", "cC", "cC", "cC", "cC")
	c.sprite(x+2, y+11, pal, "cccccccc")
	c.sprite(x+5, y+12, pal, "C", "C")
	c.sprite(x+3, y+14, pal, "cc.cc", "k...k")
	c.sprite(x+6, y+10, pal,
		"pppppppp",
		"PPPPPPPp",
		"......pp",
		"......pP",
		"......kkk")
	c.sprite(x+3, y+6, pal,
		"...oo...",
		"..oTtto.",
		".oTTtttto",
		".oTTtLtto",
		".oTTtLtto")
	head := []string{
		"..oooo..",
		".ohhhHo.",
		"ohhhhhHo",
		"ohdsssso",
		"ohssseso",
		".oSsssmo",
		"..oSSoo.",
	}
	if frame%17 == 0 {
		head[4] = "ohsssSso" // a blink
	}
	switch a.act {
	case actDone:
		head[5] = ".oSssmmo"
	case actFail:
		head[4] = "ohssSeSo"
	}
	c.sprite(x+3, y, pal, head...)
}

// drawArms draws the arms and what the hands hold, in front of the desk.
func drawArms(c *canvas, x, y int, a *actor, frame int, now time.Time) {
	pal := person(a)
	x += walkIn(a, now)
	arm := func(rows ...string) { c.sprite(x+8, y+8, pal, rows...) }
	switch a.act {
	case actWrite, actRun:
		// Typing: the hands take turns on the keyboard.
		if frame%2 == 0 {
			arm("tTTTTs", "......")
		} else {
			arm("tTTTT.", ".....s")
		}
	case actRead, actPlan:
		page := map[rune]rgb{'#': colPaper, '-': colInk, 'b': colBoard, 'v': colGreen, 'o': colOutline}
		if a.act == actRead {
			lines := []string{"oooooo", "o####o", "o#--#o", "o#-##o", "o#--#o", "oooooo"}
			if (frame/10)%2 == 1 {
				lines = []string{"oooooo", "o####o", "o#-##o", "o#--#o", "o#-##o", "oooooo"}
			}
			c.sprite(x+10, y+3, page, lines...)
		} else {
			tick := (frame / 4) % 4
			rows := []string{"oobboo", "o####o", "o#--#o", "o#--#o", "o#--#o", "oooooo"}
			for j := 1; j <= tick && j < 4; j++ {
				rows[j+1] = "o#v-#o"
			}
			c.sprite(x+10, y+3, page, rows...)
		}
		c.sprite(x+9, y+7, pal, "Tts", "...")
	case actSearch:
		off := []int{0, 1, 0, -1}[frame%4]
		c.sprite(x+12+off, y+2, map[rune]rgb{'o': colFrame, 'g': colMagnify, 'G': shade(colMagnify, 1.1)},
			".oo.", "oGgo", "oggo", ".oo.")
		c.sprite(x+11+off, y+6, map[rune]rgb{'o': colFrame}, "o")
		c.sprite(x+9, y+7, pal, "Tts")
	case actWeb:
		arm("tTTTTTTTTs") // on the mouse
		c.dot(x+19, y+8, colKeyCap)
	case actDelegate:
		reach := (frame / 2) % 2
		c.sprite(x+12+reach, y+5, map[rune]rgb{'#': colFolder, '=': shade(colFolder, 0.8)}, "##..", "####", "====")
		c.sprite(x+9, y+7, pal, "TTts")
	case actThink:
		c.sprite(x+8, y+5, pal, "s", "t", "t")
		bub := map[rune]rgb{'#': colBubble}
		switch frame % 4 {
		case 3:
			c.sprite(x+14, y, bub, "###")
			fallthrough
		case 2:
			c.sprite(x+12, y+1, bub, "##")
			fallthrough
		case 1:
			c.sprite(x+11, y+3, bub, "#")
		}
	case actWait:
		c.sprite(x+11, y, map[rune]rgb{'#': colYellow, 'o': colOutline},
			"ooooo", "o###o", "oo.#o", "oo##o", "ooooo", "oo#oo", ".ooo.")
		arm("tTs")
	case actDone:
		c.sprite(x+2, y+2, pal, "s", "t", "t")
		c.sprite(x+11, y+2, pal, "s", "t", "t")
	case actFail:
		c.sprite(x+10, y+1, pal, "s", "t", "t")
		arm("tTs")
	default: // idle: a hand by the mug
		arm("tTTTTTTs")
	}
}

// drawMonitor fills a w×h screen with what the actor is doing.
func drawMonitor(c *canvas, x, y, w, h int, act string, frame int) {
	switch act {
	case actWrite:
		c.rect(x, y, w, h, colScreen)
		cols := []rgb{colCodeA, colCodeB, colCodeC, colCodeA}
		n := w + frame%(w*h/2) // a screenful being written, never blank
		for j := 0; j < h; j++ {
			ind := []int{0, 1, 1, 0}[j%4]
			ln := min(max(n-j*3, 0), w-1-ind)
			c.rect(x+ind, y+j, ln, 1, cols[j%len(cols)])
		}
		if frame%2 == 0 {
			c.dot(x+min(max(n-(h-1)*3, 0), w-1), y+h-1, colBubble)
		}
	case actRun:
		c.rect(x, y, w, h, colTerm)
		for j := 0; j < h; j++ {
			ln := 1 + (frame+j*5)%(w-1)
			col := colGreen
			if (frame+j)%7 == 0 {
				col = colYellow
			}
			c.rect(x, y+j, ln, 1, shade(col, 0.85))
		}
		c.dot(x, y+h-1, colGreen)
		if frame%2 == 0 {
			c.dot(x+2, y+h-1, colBubble)
		}
	case actRead, actSearch, actPlan:
		c.rect(x, y, w, h, colPaper)
		for j := 0; j < h; j++ {
			c.rect(x+1, y+j, 2+(j*3+frame/4)%(w-3), 1, colInk)
		}
	case actWeb:
		c.rect(x, y, w, h, rgb{236, 240, 248})
		c.rect(x, y, w, 1, colBlue)
		c.dot(x+w-1, y, colRed)
		c.rect(x+1, y+1, 3, 2, rgb{150, 190, 240})
		for j := 1; j < h; j++ {
			c.rect(x+5, y+j, 1+(frame+j)%3, 1, colInk)
		}
	case actDone:
		c.rect(x, y, w, h, colScreen)
		c.sprite(x+(w-5)/2, y, map[rune]rgb{'#': colGreen}, "....#", "...#.", "#.#..", ".#...")
	case actFail:
		c.rect(x, y, w, h, colScreen)
		c.sprite(x+(w-4)/2, y, map[rune]rgb{'#': colRed}, "#..#", ".##.", ".##.", "#..#")
	case actThink, actDelegate, actWait:
		c.rect(x, y, w, h, colScreen)
		c.dot(x+1+frame%(w-2), y+h/2, colCodeA)
		c.dot(x+1+(frame+3)%(w-2), y+h/2-1, colCodeC)
	default:
		c.rect(x, y, w, h, colScreen)
		// A slow screensaver.
		c.dot(x+(frame/4)%w, y+(frame/7)%h, colCodeC)
	}
}

// staffScene draws a staff member's smaller desk.
func staffScene(a *actor, frame int, now time.Time) *canvas {
	c := newCanvas(staffW, staffH)
	c.rect(0, 0, staffW, 8, colWall)
	c.rect(0, 5, staffW, 1, colRail)
	c.rect(0, 6, staffW, 2, colWainscot)
	c.rect(0, 8, staffW, 1, colBase)
	c.rect(0, 9, staffW, 3, colFloorA)
	c.rect(0, 11, staffW, 1, colFloorC)
	// Desk and monitor.
	c.rect(8, 7, 8, 1, colDeskHi)
	c.rect(8, 8, 8, 1, colDeskFace)
	c.rect(9, 9, 1, 2, colDeskLeg)
	c.rect(14, 9, 1, 2, colDeskLeg)
	c.rect(10, 1, 6, 5, colBezel)
	c.rect(10, 1, 6, 1, colBezelHi)
	drawMonitor(c, 11, 2, 4, 3, a.act, frame)
	c.rect(12, 6, 2, 1, colBezelHi)

	pal := person(a)
	x := walkIn(a, now)
	c.sprite(x+1, 4, pal, "c", "C", "C", "C")
	c.sprite(x+1, 8, pal, "cccccc", "..C...", ".c.c..")
	c.sprite(x+3, 7, pal, "pppppp", ".....p", ".....kk")
	c.sprite(x+2, 5, pal, "oTttto", "oTtLto")
	head := []string{
		"..ooo..",
		".ohhHo.",
		"ohhssso",
		"ohsseso",
		".oSssmo",
	}
	if frame%19 == 0 {
		head[3] = "ohssSso"
	}
	c.sprite(x+1, 0, pal, head...)
	switch a.act {
	case actWrite, actRun:
		if frame%2 == 0 {
			c.sprite(x+6, 5, pal, "Tts")
		} else {
			c.sprite(x+6, 5, pal, "Tt.", "..s")
		}
	case actDone:
		c.sprite(x+1, 0, pal, "s", "t")
		c.sprite(x+7, 0, pal, "s", "t")
	case actThink:
		c.dot(x+8, 0, colBubble)
		if frame%2 == 0 {
			c.dot(x+9, 0, colBubble)
		}
	default:
		c.sprite(x+6, 5, pal, "Tts")
	}
	return c
}
