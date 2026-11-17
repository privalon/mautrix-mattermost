package mattermostfmt

import (
	"strings"
	"testing"
)

func TestParseNumberedListWithContinuations(t *testing.T) {
	in := "Decisions needed\n1. First\n   (cont one)\n2. Second\n   cont two\n3. Third `x`\n\nafter"
	got := Parse(in).FormattedBody
	if n := strings.Count(got, "<ol"); n != 1 {
		t.Fatalf("want 1 <ol>, got %d: %s", n, got)
	}
	if strings.Count(got, "<li>") != 3 || !strings.Contains(got, "<br/>(cont one)</li>") {
		t.Fatalf("bad items: %s", got)
	}
	if strings.Contains(got, "<br/><ol") || strings.Contains(got, "</ol><br/>") {
		t.Fatalf("stray br around list: %s", got)
	}
}

func TestParseOrderedListStart(t *testing.T) {
	got := Parse("3. a\n4. b").FormattedBody
	if !strings.Contains(got, `<ol start="3">`) {
		t.Fatalf("missing start: %s", got)
	}
}
