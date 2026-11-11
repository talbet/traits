package diff

import "strings"

// maxLCSCells bounds the line-diff table (old lines x new lines, after
// trimming the common prefix and suffix). Larger changes are reported as the
// whole changed region replaced.
const maxLCSCells = 1 << 22

// Synthetic builds a File for path by diffing oldContent against newContent
// line by line, with context unchanged lines around each change (negative
// means 0). Both OldPath and NewPath are path. Identical contents give a File
// with no hunks. A missing final newline is not distinguished.
func Synthetic(path, oldContent, newContent string, context int) File {
	context = max(context, 0)
	script := lineScript(splitContent(oldContent), splitContent(newContent))
	f := File{OldPath: path, NewPath: path}

	var oldBefore, newBefore int // old/new lines before script[i]
	counts := make([][2]int, len(script)+1)
	for i, l := range script {
		counts[i] = [2]int{oldBefore, newBefore}
		if l.Kind != Added {
			oldBefore++
		}
		if l.Kind != Removed {
			newBefore++
		}
	}
	counts[len(script)] = [2]int{oldBefore, newBefore}

	for i := 0; i < len(script); {
		if script[i].Kind == Context {
			i++
			continue
		}
		begin := max(i-context, 0)
		// Extend past changes separated by at most 2*context unchanged lines.
		end, gap := i, 0
		for j := i; j < len(script) && gap <= 2*context; j++ {
			if script[j].Kind == Context {
				gap++
			} else {
				end, gap = j, 0
			}
		}
		stop := min(end+1+context, len(script))
		h := hunkOf(script[begin:stop], counts[begin])
		f.Hunks = append(f.Hunks, h)
		i = stop
	}
	return f
}

// hunkOf wraps lines as a hunk whose first line follows before[0] old and
// before[1] new lines.
func hunkOf(lines []Line, before [2]int) Hunk {
	h := Hunk{Lines: lines}
	for _, l := range lines {
		if l.Kind != Added {
			h.OldLines++
		}
		if l.Kind != Removed {
			h.NewLines++
		}
	}
	h.OldStart, h.NewStart = before[0], before[1]
	if h.OldLines > 0 {
		h.OldStart++
	}
	if h.NewLines > 0 {
		h.NewStart++
	}
	return h
}

func splitContent(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// lineScript returns the edit script turning a into b as hunk lines with
// line numbers, using a longest-common-subsequence table over the region
// between the common prefix and suffix.
func lineScript(a, b []string) []Line {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	ma, mb := a[pre:len(a)-suf], b[pre:len(b)-suf]

	var script []Line
	oldNo, newNo := 1, 1
	emit := func(k Kind, text string) {
		l := Line{Kind: k, Text: text}
		if k != Added {
			l.OldLine = oldNo
			oldNo++
		}
		if k != Removed {
			l.NewLine = newNo
			newNo++
		}
		script = append(script, l)
	}

	for _, s := range a[:pre] {
		emit(Context, s)
	}
	if len(ma)*len(mb) > maxLCSCells {
		for _, s := range ma {
			emit(Removed, s)
		}
		for _, s := range mb {
			emit(Added, s)
		}
	} else {
		// lcs[i][j] is the LCS length of ma[i:] and mb[j:].
		w := len(mb) + 1
		lcs := make([]int32, (len(ma)+1)*w)
		for i := len(ma) - 1; i >= 0; i-- {
			for j := len(mb) - 1; j >= 0; j-- {
				if ma[i] == mb[j] {
					lcs[i*w+j] = lcs[(i+1)*w+j+1] + 1
				} else {
					lcs[i*w+j] = max(lcs[(i+1)*w+j], lcs[i*w+j+1])
				}
			}
		}
		i, j := 0, 0
		for i < len(ma) || j < len(mb) {
			switch {
			case i < len(ma) && j < len(mb) && ma[i] == mb[j]:
				emit(Context, ma[i])
				i++
				j++
			case i < len(ma) && (j == len(mb) || lcs[(i+1)*w+j] >= lcs[i*w+j+1]):
				emit(Removed, ma[i])
				i++
			default:
				emit(Added, mb[j])
				j++
			}
		}
	}
	for _, s := range a[len(a)-suf:] {
		emit(Context, s)
	}
	return script
}
