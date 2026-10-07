package engine

import (
	"strings"
	"testing"
)

const essay = "In today's fast-paced digital landscape, it is important to note that education must leverage robust technology to ensure seamless learning. Moreover, a comprehensive curriculum is crucial for navigating the multifaceted challenges students face. Furthermore, schools that utilize adaptive tools can facilitate better outcomes. In conclusion, we cannot ignore how pivotal these tools are to long-term success."

func TestDetectEssay(t *testing.T) {
	r := Detect(essay)
	if r.Score != 95 {
		t.Fatalf("score = %d, want 95", r.Score)
	}
	if len(r.Sentences) != 4 {
		t.Fatalf("sentences = %d, want 4", len(r.Sentences))
	}
	want := []int{98, 98, 98, 86}
	for i, sc := range want {
		s := r.Sentences[i]
		if s.Score != sc || !s.Flagged {
			t.Fatalf("sentence %d = %+v, want score %d flagged", i, s, sc)
		}
		if s.Note == "" || !strings.Contains(s.Note, "classic AI phrasing") {
			t.Fatalf("sentence %d note = %q", i, s.Note)
		}
	}
}

func TestHumanizeEssay(t *testing.T) {
	got := Humanize(essay, Balanced)
	want := "These days, education must use solid technology to make sure smooth learning. Plus, a full curriculum is key for handling the tricky challenges students face. And schools that use adaptive tools can drive better outcomes. So, we can't ignore how big these tools are to long-term success."
	if got != want {
		t.Fatalf("humanize =\n%q\nwant\n%q", got, want)
	}
	after := Detect(got)
	if after.Score != 27 {
		t.Fatalf("after score = %d, want 27", after.Score)
	}
	for _, s := range after.Sentences {
		if s.Flagged {
			t.Fatalf("rewritten sentence still flagged: %+v", s)
		}
	}
}

func TestCasualAddsRules(t *testing.T) {
	in := "However, we are going to do this in order to help."
	if Humanize(in, Academic) == Humanize(in, Casual) {
		t.Fatal("casual should rewrite more than academic")
	}
	got := Humanize(in, Casual)
	if !strings.Contains(got, "But ") || !strings.Contains(got, "we're") || strings.Contains(got, "in order to") {
		t.Fatalf("casual = %q", got)
	}
}

func TestCheckText(t *testing.T) {
	if _, err := CheckText("   "); err == nil {
		t.Fatal("empty text accepted")
	}
	long := strings.Repeat("a", MaxRunes+1)
	if _, err := CheckText(long); err == nil {
		t.Fatal("oversize text accepted")
	}
}

func TestChunksKeepText(t *testing.T) {
	s := "These days, education must use solid technology to make sure smooth learning."
	parts := Chunks(s, 24)
	if len(parts) < 2 {
		t.Fatalf("parts = %#v", parts)
	}
	joined := ""
	for _, p := range parts {
		joined += p
	}
	if joined != s {
		t.Fatalf("joined = %q", joined)
	}
}
