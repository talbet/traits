package diff

import (
	"strings"
	"testing"
)

// FuzzParse checks that Parse never panics and that every parsed hunk's
// lines agree with its header counts and are numbered consecutively.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		modify,
		"--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n a\n-b\n+c\n",
		"diff --git a/foo bar.go b/foo bar.go\n--- a/foo bar.go\t\n+++ b/foo bar.go\t\n@@ -1 +1 @@\n-a\n+b\n",
		"diff --git \"a/t\\tab.go\" \"b/t\\tab.go\"\nrename from \"t\\tab.go\"\nrename to x\n",
		"diff --git a/d b/d\ndeleted file mode 100644\n--- a/d\n+++ /dev/null\n@@ -1 +0,0 @@\n-a\n\\ No newline at end of file\n",
		"diff --git a/i b/i\nBinary files a/i and b/i differ\n",
		"--- a/f\r\n+++ b/f\r\n@@ -0,0 +1 @@\r\n+x\r\n",
		"@@ -1 +1 @@\n",
		"",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, src string) {
		files, err := Parse(strings.NewReader(src))
		if err != nil {
			return
		}
		if len(files) == 0 {
			t.Fatal("no files and no error")
		}
		for _, file := range files {
			checkLineNumbers(t, file)
			_ = file.ChangedText()
			_ = file.Path()
		}
	})
}

// FuzzSynthetic checks that Synthetic's hunks rebuild the new content and
// parse back unchanged.
func FuzzSynthetic(f *testing.F) {
	f.Add("a\nb\nc\n", "a\nx\nc\n", 3)
	f.Add("", "a\n", 0)
	f.Add("1\n2\n3\n4\n5\n6\n7\n8\n", "0\n2\n3\n4\n5\n6\n7\n9\n", 1)
	f.Fuzz(func(t *testing.T, old, new string, context int) {
		context = context % 8
		if strings.ContainsRune(old+new, '\r') {
			return // Parse normalizes CRLF, so the text would not round-trip.
		}
		checkSynthetic(t, Synthetic("p", old, new, context), old, new, max(context, 0))
	})
}
