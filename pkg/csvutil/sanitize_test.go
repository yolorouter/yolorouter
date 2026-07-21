package csvutil

import "testing"

func TestSanitizeCellPrefixesFormulaChars(t *testing.T) {
	tests := []struct{ in, want string }{
		{"=SUM(A1:A2)", "\t=SUM(A1:A2)"},
		{"+1+1", "\t+1+1"},
		{"-1", "\t-1"},
		{"@cmd|' /c calc'!A1", "\t@cmd|' /c calc'!A1"},
		{"normal model name", "normal model name"},
		{"gpt-4o", "gpt-4o"}, // hyphen mid-string is fine, only leading '-' is dangerous
		{"", ""},
		{"价格未知", "价格未知"},
		// Adversarial: leading control/whitespace bytes must NOT evade the
		// formula check (Codex adversarial finding). The sanitizer strips them
		// first, so =/+/-/@ is still detected and the cell is text-prefixed.
		{"\t=evil", "\t=evil"}, // leading tab stripped, = detected, re-prefixed
		{"\n=evil", "\t=evil"},
		{"\r=evil", "\t=evil"},
		{" =evil", "\t=evil"},
		{"\t\n\r=evil", "\t=evil"},
		// Leading control on a non-formula cell is left intact (preserves the
		// original value; csv.Writer quotes cells containing tabs/newlines).
		{"\tnot-formula", "\tnot-formula"},
	}
	for _, tt := range tests {
		if got := SanitizeCell(tt.in); got != tt.want {
			t.Errorf("SanitizeCell(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSanitizeRowAppliesToEveryCell(t *testing.T) {
	row := []string{"=evil", "safe", "+1", ""}
	got := SanitizeRow(row)
	want := []string{"\t=evil", "safe", "\t+1", ""}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("row[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
