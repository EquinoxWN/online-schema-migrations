package sqlscan

import (
	"strings"
	"testing"
)

func TestSplitRespectsQuotesCommentsAndLines(t *testing.T) {
	src := "-- header; not a statement\n" +
		"CREATE TABLE a (x text DEFAULT 'semi;colon');\n" +
		"/* block; /* nested; */ still comment */\n" +
		"CREATE FUNCTION f() RETURNS int AS $body$ SELECT 1; $body$ LANGUAGE sql;\n" +
		"ALTER TABLE \"we;ird\" ADD COLUMN y int;\n" +
		"SELECT E'it\\'s;';\n" +
		"SELECT $1;"
	got, err := Split(src)
	if err != nil {
		t.Fatal(err)
	}
	wantLines := []int{2, 4, 5, 6, 7}
	if len(got) != len(wantLines) {
		t.Fatalf("got %d statements, want %d: %q", len(got), len(wantLines), got)
	}
	for i, st := range got {
		if st.Line != wantLines[i] {
			t.Errorf("statement %d starts on line %d, want %d (%q)", i, st.Line, wantLines[i], st.Text)
		}
	}
	if !strings.Contains(got[1].Text, "SELECT 1; $body$") {
		t.Errorf("dollar-quoted body was split: %q", got[1].Text)
	}
}

func TestSplitEmptyAndOnlyComments(t *testing.T) {
	for _, src := range []string{"", "  ;; \n", "-- just a comment", "/* x */ ;"} {
		got, err := Split(src)
		if err != nil || len(got) != 0 {
			t.Errorf("Split(%q) = %v, %v; want no statements", src, got, err)
		}
	}
}

func TestSplitRejectsUnterminated(t *testing.T) {
	for _, src := range []string{"SELECT 'open", `SELECT "open`, "SELECT $a$ open", "SELECT 1 /* open"} {
		if _, err := Split(src); err == nil {
			t.Errorf("Split(%q) accepted unterminated input", src)
		}
	}
}

func TestSplitRejectsOversizedInput(t *testing.T) {
	if _, err := Split(strings.Repeat(" ", MaxInput+1)); err != ErrTooLarge {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
}

func TestTokensFoldsWordsAndKeepsQuotedNames(t *testing.T) {
	toks, err := Tokens(`ALTER TABLE "My""Table" ADD x numeric(10,2) DEFAULT e'a\'b' -- c`)
	if err != nil {
		t.Fatal(err)
	}
	want := []Token{{Word, "alter"}, {Word, "table"}, {Ident, `My"Table`}, {Word, "add"}, {Word, "x"},
		{Word, "numeric"}, {Punct, "("}, {Number, "10"}, {Punct, ","}, {Number, "2"}, {Punct, ")"},
		{Word, "default"}, {String, `e'a\'b'`}}
	if len(toks) != len(want) {
		t.Fatalf("got %v, want %v", toks, want)
	}
	for i := range want {
		if toks[i] != want[i] {
			t.Errorf("token %d = %v, want %v", i, toks[i], want[i])
		}
	}
	if !toks[0].Is("ALTER") || toks[2].Is("my\"table") {
		t.Error("Is must match words case-insensitively and never match quoted identifiers")
	}
}

func FuzzSplit(f *testing.F) {
	for _, s := range []string{"SELECT 1;", "$$;$$", "'a''b';", "/* /* */ */;", `"x"";"`, "E'\\'';"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		stmts, err := Split(s)
		if err != nil {
			return
		}
		for _, st := range stmts {
			if st.Line < 1 || strings.TrimSpace(st.Text) == "" {
				t.Fatalf("bad statement %+v", st)
			}
			_, _ = Tokens(st.Text) // must not panic; errors are allowed
		}
	})
}
