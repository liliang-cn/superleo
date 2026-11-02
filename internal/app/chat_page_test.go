package app

import (
	"fmt"
	"testing"

	"github.com/liliang-cn/superleo/internal/backend"
)

func conv(pairs int) []backend.ChatTurn {
	var out []backend.ChatTurn
	for i := 0; i < pairs; i++ {
		out = append(out,
			backend.ChatTurn{Role: "user", Content: fmt.Sprintf("q%d", i)},
			backend.ChatTurn{Role: "user", Kind: backend.TurnKindContext, Content: "memory"},
			backend.ChatTurn{Role: "assistant", Content: fmt.Sprintf("a%d", i)})
	}
	return out
}

func TestPageTurnsNewestThenEarlier(t *testing.T) {
	all := conv(50) // 100 turns without context
	p := pageTurns(all, -1, 10)
	if p.Total != 100 || len(p.Turns) != 10 || p.Start != 90 || !p.More {
		t.Fatalf("newest page: %+v", p)
	}
	if p.Turns[0].Content != "q45" || p.Turns[9].Content != "a49" {
		t.Fatalf("newest page holds %q..%q", p.Turns[0].Content, p.Turns[9].Content)
	}
	for _, x := range p.Turns {
		if x.Kind == backend.TurnKindContext {
			t.Fatal("context turn in a page")
		}
	}
	seen := len(p.Turns)
	for p.More {
		prev := p.Start
		p = pageTurns(all, prev, 10)
		if p.Start >= prev || p.Turns[0].Role != "user" {
			t.Fatalf("earlier page %+v", p)
		}
		seen += len(p.Turns)
	}
	if seen != 100 || p.Start != 0 {
		t.Fatalf("pages cover %d turns, ending at %d", seen, p.Start)
	}
}

func TestPageTurnsOddLimitStartsOnQuestion(t *testing.T) {
	p := pageTurns(conv(10), -1, 5)
	if p.Turns[0].Role != "user" || len(p.Turns) != 6 {
		t.Fatalf("page opens on %q with %d turns", p.Turns[0].Content, len(p.Turns))
	}
}

func TestPageTurnsShortAndEmpty(t *testing.T) {
	if p := pageTurns(nil, -1, 0); p.More || len(p.Turns) != 0 || p.Total != 0 {
		t.Fatalf("empty: %+v", p)
	}
	p := pageTurns(conv(3), -1, 0)
	if p.More || len(p.Turns) != 6 || p.Start != 0 {
		t.Fatalf("short: %+v", p)
	}
	if p := pageTurns(conv(3), 0, 10); len(p.Turns) != 0 || p.More {
		t.Fatalf("before 0: %+v", p)
	}
	if p := pageTurns(conv(3), 999, 10); len(p.Turns) != 6 {
		t.Fatalf("before past the end: %+v", p)
	}
}

func TestPageTurnsLimitClamped(t *testing.T) {
	if p := pageTurns(conv(300), -1, 100000); len(p.Turns) > 2*maxPageTurns {
		t.Fatalf("page of %d", len(p.Turns))
	}
}
