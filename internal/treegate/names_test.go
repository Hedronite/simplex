// Package treegate holds the test that keeps internal names out of the public
// tree. It has no production code.
//
// The rule: no tracked file, and no tracked path, may contain a whole word
// from a list of names of internal machines, seats and tools, in any case.
// The list is held as SHA-256 hashes of the lowercased words, so this file
// does not publish the names it forbids, and a failure reports the path, the
// line and a hash prefix, never the word. To add a name:
//
//	printf '%s' name | shasum -a 256
//
// and add the digest below. A word is a maximal run of ASCII letters and
// digits, so a hyphenated or underscored identifier is checked piece by
// piece. A genuine, reviewed use of a listed word in ordinary prose is
// allowed by adding its "path|first-16-hex-of-hash" to allowed.
package treegate

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// forbidden holds the hashes of the lowercased words.
var forbidden = map[string]bool{
	"020dc2295b4ca69a44356c864ff86aa3cb8026d7ce3b64db402cfaf7da4915e4": true,
	"26f8567f2569182294c3fa5b9f9cb2270b554eef628b4c149cf82a42888ff4ae": true,
	"27037fccea3062ee8ebaea07a9e2bf8dcb6511fd860ae993442aee0c512b8bbf": true,
	"38dbfe0f056f7846834b9b68abb7cc8e4d2ab1f27f287caffbabb98e74915356": true,
	"3cdea1b06e8f4c08213ca4cbd702a27871914c971ee226cf9debf88b8435c375": true,
	"3e3122e4a6fa01d8ebac41e91ab6d57caa8f77d23af4a45c67689f2b3f45baa0": true,
	"3f8d7379e2f0f7d0c845a06f7b67ef80cacce0c686254aced769c1698f3fe59a": true,
	"40b5553ca09e063dd656c832e267f9914c07e2f6fdd22213588b4ad09aed198d": true,
	"4e2295dd929e424aa1afde7049924cb231f451c9884d7915ae33690b73b227ec": true,
	"56bf6664d56eec6fc29d79695884dc6e841aa3c895c633a6155086535e948a35": true,
	"5c07ce13958f56fdb7c9c5436785723f50e63294db365bec4cd25a89d7d9fb19": true,
	"6b27eba5652b46d31ef75f17784c2709ac019809c47ca538e91c0ab54a80892f": true,
	"73c46de9117a51af2a6def9a7dbc1a5413cba8e5fb520474ed1029d50c774616": true,
	"7f27fbfd9e241df13b7fa9d88c8db3208c836f783455238a5417553657061bf8": true,
	"82a208225582b3b2d6cfb16e9aaae8ecc38033b46cffa79ae754a1b4c28a6f4d": true,
	"8c3d8008e1a8898b18b0a2e701333c8ea9b44320640fd8db9dedd720b25d14f1": true,
	"913a53d7df2fad651bed414afb045378c3495b23d5174bead8091dcc8cdef8ff": true,
	"a4e63bcacf6c172ad84f9f4523c8f1acaf33676fa76d3258c67b7e7bbf16d777": true,
	"ad2015263f339bff96818238148b790bd02be561472f4ea7db44498caa0b6558": true,
	"ae74f72d212fb9871302a2459aeaf7b20bc2f792e4852be648a7d4e63967d9b1": true,
	"c737e8142a927072977437977d3846acf36f5057d167cf45ea95c7423ed27a0f": true,
	"dfb316701857783dac69a14d1fe3fd60cff21d56e830baf7f0e3871bd73eee39": true,
	"e670e2cbf7f657890bf01635ace907c32e35d022daaa89c1fb0b18304ec24abe": true,
}

// allowed lists reviewed exceptions as "path|hash prefix". It is empty on
// purpose: an entry here is a decision someone has to defend in review.
var allowed = map[string]bool{}

const maxFileBytes = 4 << 20

func hashWord(w string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(w)))
	return hex.EncodeToString(sum[:])
}

// words splits s into maximal runs of ASCII letters and digits.
func words(s string) []string {
	var out []string
	start := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		isWord := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		switch {
		case isWord && start < 0:
			start = i
		case !isWord && start >= 0:
			out = append(out, s[start:i])
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, s[start:])
	}
	return out
}

// mask returns s with every listed word replaced by ***, so a path that itself
// contains a listed word can still be shown in a finding.
func mask(s string, set map[string]bool) string {
	var b strings.Builder
	last := 0
	for _, w := range words(s) {
		i := strings.Index(s[last:], w) + last
		b.WriteString(s[last:i])
		if set[hashWord(w)] {
			b.WriteString("***")
		} else {
			b.WriteString(w)
		}
		last = i + len(w)
	}
	b.WriteString(s[last:])
	return b.String()
}

// scan returns one finding per listed word in a path or in a file's lines.
// A finding never contains the word: the path is shown with listed words
// masked, and the line is identified by number.
func scan(path string, content []byte, set, allow map[string]bool) []string {
	var found []string
	shown := mask(path, set)
	check := func(where string, text string) {
		for _, w := range words(text) {
			h := hashWord(w)
			if !set[h] || allow[path+"|"+h[:16]] {
				continue
			}
			found = append(found, fmt.Sprintf("%s: a word on the internal-names list (sha256 %s)", where, h[:16]))
		}
	}
	check(shown+" (path)", path)
	sc := bufio.NewScanner(bytes.NewReader(content))
	sc.Buffer(make([]byte, 0, 64<<10), maxFileBytes)
	line := 0
	for sc.Scan() {
		line++
		check(fmt.Sprintf("%s:%d", shown, line), sc.Text())
	}
	return found
}

func TestMechanismFindsAListedWordInAnyCase(t *testing.T) {
	// A harmless stand-in, so the mechanism is tested without naming anything.
	set := map[string]bool{hashWord("zzsample"): true}
	for _, in := range []string{"zzsample", "ZZSample", "a ZZSAMPLE b", "x-zzsample-y", "x_zzsample.y"} {
		if got := scan("f.txt", []byte(in+"\n"), set, nil); len(got) != 1 {
			t.Errorf("%q: %d findings, want 1: %v", in, len(got), got)
		}
	}
	for _, in := range []string{"zzsamples", "azzsample", "zzsampl", "sample", ""} {
		if got := scan("f.txt", []byte(in+"\n"), set, nil); len(got) != 0 {
			t.Errorf("%q: false positive: %v", in, got)
		}
	}
	// The path is scanned too, and no finding ever repeats the word, even when
	// the word is in the path itself.
	got := scan("dir/zzsample.txt", []byte("zzsample\n"), set, nil)
	if len(got) != 2 {
		t.Fatalf("path and line should each be reported once: %v", got)
	}
	for _, f := range got {
		if strings.Contains(strings.ToLower(f), "zzsample") {
			t.Fatalf("a finding repeats the word: %q", f)
		}
	}
	if m := mask("a/ZZSample-x/zzsample.go", set); m != "a/***-x/***.go" {
		t.Fatalf("mask = %q", m)
	}

	// An allow entry silences exactly that path and word.
	allow := map[string]bool{"f.txt|" + hashWord("zzsample")[:16]: true}
	if got := scan("f.txt", []byte("zzsample\n"), set, allow); len(got) != 0 {
		t.Errorf("allowed entry still reported: %v", got)
	}
	if got := scan("g.txt", []byte("zzsample\n"), set, allow); len(got) != 1 {
		t.Errorf("allow entry leaked to another path: %v", got)
	}
}

func TestTheListIsWellFormed(t *testing.T) {
	if len(forbidden) == 0 {
		t.Fatal("the list is empty")
	}
	for h := range forbidden {
		if len(h) != 64 || strings.Trim(h, "0123456789abcdef") != "" {
			t.Fatalf("%q is not a lowercase SHA-256 digest", h)
		}
	}
}

// trackedFiles lists the tracked paths under root. It is an error, not an
// empty list, when root is not a repository: a gate that finds nothing to scan
// has not passed.
func trackedFiles(root string) ([]string, error) {
	cmd := exec.Command("git", "ls-files", "-z")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files in %s: %w", root, err)
	}
	var files []string
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel != "" {
			files = append(files, rel)
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("git ls-files in %s listed no files", root)
	}
	return files, nil
}

func TestNoRepositoryIsAnErrorNotAnEmptyPass(t *testing.T) {
	if files, err := trackedFiles(t.TempDir()); err == nil {
		t.Fatalf("a directory that is not a repository listed %d file(s) without an error", len(files))
	}
}

// TestNoInternalNamesInTrackedFiles is the gate. It never skips: if there is no
// repository to list, it fails, so a source archive or a CI checkout without
// history cannot read as a pass.
func TestNoInternalNamesInTrackedFiles(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	files, err := trackedFiles(root)
	if err != nil {
		t.Fatalf("the gate has nothing to scan: %v", err)
	}
	var findings []string
	for _, rel := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxFileBytes {
			findings = append(findings, scan(rel, nil, forbidden, allowed)...)
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if bytes.IndexByte(content, 0) >= 0 {
			content = nil // binary: only its path is checked
		}
		findings = append(findings, scan(rel, content, forbidden, allowed)...)
	}
	if len(findings) > 0 {
		t.Fatalf("%d use(s) of a word on the internal-names list in the tracked tree:\n%s",
			len(findings), strings.Join(findings, "\n"))
	}
}
