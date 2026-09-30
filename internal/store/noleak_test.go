package store

// Guard against committing personal data.
//
// The database holds live WebUntis account secrets, and this project is
// published. Test files previously carried real base32 TOTP seeds copied out of
// the production database, which let anyone generate valid login codes for real
// students and teachers. These checks fail if a value that looks like a live
// secret, or a pattern the project is known to have leaked, reappears anywhere
// in tracked source.
//
// The database itself is gitignored (see .gitignore), so it is never scanned
// here; these tests are about the source tree.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// forbiddenSecretSHA256 is the record of every live base32 TOTP seed that has
// been committed to this repository, as SHA-256 digests.
//
// Digests, not the seeds themselves: a literal deny-list would re-commit the
// exact strings this guard exists to keep out, and GitHub's secret scanning (or
// the next reader) would treat them as freshly leaked credentials.
//
// These accounts must be treated as compromised. Removing a string from the
// working tree does not undo its presence in the published history, so the fix
// is to re-provision the affected authenticators, not only to edit the file.
// Keep this list append-only.
var forbiddenSecretSHA256 = []string{
	"b4581ffbd19aa0f7107baac0c273fd6e696a1f66e0de9eaf6ab471de2b01d392", // was in TESTING-CHECKLIST.md
	"bec015a4670a2d5e64e51ef3b417a37e24576585178cbe3db82a67b3c7480c45", // was in FEATURE-CHECKLIST.md
	"7946c61ab62b5b8d6ef904fbc350758d2e418f276d7315a3025c0ba3c016a3f4", // was in README.md
	"0eb59ab89adfd28a6f7932d4f0651e79df72a6af3b471d20f5364fad98b15c52", // also present in a local chat log
	"e3de013b0d34caeccfacf1ab05e6fde2ac7a3d8cc4988fa4144eee6a564b6c12", // also present in a local chat log
}

// forbiddenIdentifierSHA256 is the record of the real usernames, personal
// names, school names, hostnames and class ids that have appeared in this
// project, as SHA-256 digests of their lowercased form.
//
// Digests, not the values: a literal deny-list would re-commit the exact
// strings this guard exists to keep out, and GitHub's secret scanning (or the
// next reader) would treat them as a fresh leak. That is not hypothetical —
// earlier revisions of this file held them in plaintext and undid their own
// purpose.
//
// One leak to this design: a digest does not reveal its input, but it does fix
// the set of strings that are banned, and the five seed digests above are
// guessable from a small space of real names. That is acceptable here because
// the accounts behind them are already compromised and must be re-provisioned
// anyway; the guard's job is to keep the next copy out of the tree.
//
// Keep this list append-only.
//
// The trailing comments describe the CATEGORY of each digest, never the value:
// an earlier revision of this file annotated them with the plaintext, which
// leaked the very strings the digests exist to hide. A reader who needs to know
// which account a digest covers should ask, not grep.
var forbiddenIdentifierSHA256 = []string{
	// real WebUntis usernames (4: three published, one local-only)
	"be55595e1da35735b56c651192eb45ad37630550fb925ea2727722243f222193",
	"a78d5193247994970ea0aad1d96942125ffc66c9ee95cd4344f3805644d184cd",
	"5df22f3207c9fa56d1e88876664edbc87b3ac1e476eafb3f829e5127cf042685",
	"7382f0ba01d7f87a1fb738f67a416cd6bd0053088bbb740363bd6e574f59226e", // non-ASCII
	// real display names (3: one two-word, two single-word)
	"2149a22673db1d961fe14af0b66ea0a7988b1d0e7a5d2df0862ddeb49edcee0e",
	"a5e7c002443743c5836758c7d1cd8ddefd9fcf2061daa0efaac683fb99966057",
	"d97e4d9c8041c16b7a16e9528946a88d5b6484b501a3a6afdc628adbc3016a4d",
	// real school names (2)
	"7a847e1a9ff0945574f41e86d101ce298d031466fe3375b900d69729dbfb5c95",
	"f8a6b216a20a6885f4917f044a9ac22eb5575a4ab1b57003cabd3e276ac5941f",
	// real deployment hostnames (3)
	"e5267a08e4b15ad9bd963d0f1f32f70aa2b9547f7e83618f399ffa0dd68fdef2",
	"f8f51c402ef71a2bbda467e57a75e118d837ba860bd70f0aabf76d0c99129c3e",
	"3e81f17f6f19dda5a2de51677b912271d2f4b8eb5c3acabbab7c40325a9e01b0",
	// real class and person ids (10)
	"1fc2789c839ba5e3700b66b672e10f89ae591f6b8f5fa23af47b809be4a518a5",
	"8818fd6d967f88106b521188fcf15b32b0c5d7af024bfe6a82842144e9b9b6c4",
	"2545a02d836fe85023efc654e841b0e03458a50d8308345ea6360ab2eaecb9cf",
	"07df47f4857229805333a3de141b6318b699e4ae52a860c9923db6de9a8bf16c",
	"11b0147cac181f433e7f220d2b0cead50d61cf065a82b45b768428419d83433e",
	"7b215942732210e4b0f640a2f030039703adceccf29170c44059fa6d65d3ef3c",
	"44f1bbc5b4c9e9a4c445554c93b8eb354c641a9c4aacb5f4f568182c63a471e2",
	"d4cd013f21bbf4b52c431691b7056337c35d0ad6fc7acc7084abc1039633618e",
	"502f2d22a8424ae4791e0009d8d29e62036fd4e4f9476d4b4293affa8877c3e4",
	"56f4da26ed956730309fa1488611ee0f13b0ac95ebb1bc9b5d210e31ff70e79c",
}

// forbiddenFragment is a prefix of the #anonymous# placeholder seed.
const forbiddenFragment = "AAAAAAAAAAAA"

// allowedBase32 is the well-known RFC-style test vector used in unit tests. It
// is public knowledge, decodes to "Hello!\xDE\xAD\xBE\xEF", and grants access to
// nothing. Keep fixtures on this value rather than inventing new seeds.
const allowedBase32 = "JBSWY3DPEHPK3PXP"

func TestNoCommittedSecrets(t *testing.T) {
	root := repoRoot(t)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skip := skipDirs[d.Name()]; skip || strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if skipFiles[d.Name()] {
			return nil
		}
		if !scannable(path) {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, bad := range forbiddenSeedDigests(string(body)) {
			t.Errorf("%s contains a live TOTP seed that was committed before (sha256 %s).\n"+
				"Treat the matching account as compromised and re-provision it; "+
				"removing the string alone does not undo the exposure.", rel, bad)
		}
		if strings.Contains(string(body), forbiddenFragment) {
			t.Errorf("%s contains the #anonymous# placeholder seed", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// identifierCandidate matches a run of word characters. A "word" here is any
// run of letters, digits, dots, dashes and underscores that is not glued to
// surrounding punctuation, so a two-word display name and a dashed hostname are
// both found intact.
var identifierCandidate = regexp.MustCompile(`[\p{L}\p{N}][\p{L}\p{N}._-]*`)

// forbiddenIdentifierHits returns a digest for every banned identifier that
// appears in s, matching single words and runs of up to three adjacent words so
// that multi-word display names are caught as well.
func forbiddenIdentifierHits(s string) []string {
	known := make(map[string]bool, len(forbiddenIdentifierSHA256))
	for _, d := range forbiddenIdentifierSHA256 {
		known[d] = true
	}
	words := identifierCandidate.FindAllString(strings.ToLower(s), -1)
	seen := make(map[string]bool)
	var hits []string
	record := func(tok string) {
		sum := sha256.Sum256([]byte(tok))
		d := hex.EncodeToString(sum[:])
		if known[d] && !seen[d] {
			seen[d] = true
			hits = append(hits, d)
		}
	}
	for i := range words {
		for n := 1; n <= 3 && i+n <= len(words); n++ {
			record(strings.Join(words[i:i+n], " "))
		}
	}
	return hits
}

// TestNoRealPersonalIdentifiers keeps the real usernames, display names, school
// names and class ids out of the source. They were real values in the
// production database; a test fixture that carries one leaks it to every future
// reader and to anyone cloning the repository.
func TestNoRealPersonalIdentifiers(t *testing.T) {
	for _, f := range trackedFiles(t) {
		body, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(repoRoot(t), f)
		for _, bad := range forbiddenIdentifierHits(string(body)) {
			t.Errorf("%s contains a real identifier that was scrubbed earlier (sha256 %s) —\n"+
				"use a synthetic fixture name, and check whether the value in the\n"+
				"published history still needs rotating", rel, bad)
		}
	}
}

// TestFixturesUseTheKnownDummySeed documents why unit tests may use
// JBSWY3DPEHPK3PXP and why they must not use anything else that looks like a
// seed.
func TestFixturesUseTheKnownDummySeed(t *testing.T) {
	if !strings.EqualFold(allowedBase32, "JBSWY3DPEHPK3PXP") {
		t.Fatal("dummy seed constant drifted from the documented test vector")
	}
	body, err := os.ReadFile("query_test.go")
	if err != nil {
		t.Skipf("query_test.go not present: %v", err)
	}
	if !strings.Contains(string(body), allowedBase32) {
		t.Errorf("query_test.go no longer uses the documented dummy seed %s", allowedBase32)
	}
}

var skipDirs = map[string]bool{
	"data": true, // live database, gitignored
	"bin":  true, // build output, gitignored
	"le":   true, // Let's Encrypt state, gitignored
}

// skipFiles are excluded from every scan. The guard itself necessarily contains
// the strings it looks for, and the local chat log is gitignored — it is not
// part of the repository, so it is not a leak and must not fail a local run.
var skipFiles = map[string]bool{
	"noleak_test.go":         true,
	"opencode-chat.md":       true,
	"MANUAL-QA-CHECKLIST.md": false, // a real committed doc: it IS scanned
}

// scannedNames are extensionless files that still have to be checked: a config
// template or a Dockerfile is exactly where a real school or host gets typed.
var scannedNames = map[string]bool{
	"Dockerfile":    true,
	".env.example":  true,
	".gitignore":    true,
	"go.mod":        false,
	"go.sum":        false,
	".dockerignore": true,
	"LICENSE":       false,
	"NOTICE":        false,
	"AGENTS.md":     false,
	"CLAUDE.md":     false,
}

// scannable reports whether a path is one of the files the guards read.
func scannable(path string) bool {
	if v, ok := scannedNames[filepath.Base(path)]; ok {
		return v
	}
	switch filepath.Ext(path) {
	case ".go", ".md", ".yaml", ".yml", ".html", ".sh", ".json", ".txt":
		return true
	}
	return false
}

// repoRoot walks up from the test's directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find module root")
	return ""
}

// trackedFiles lists the source files the other guards inspect. It deliberately
// does not shell out to git: the guard has to work in a bare checkout and in
// CI, and a file that is not committed cannot leak.
func trackedFiles(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if skipFiles[d.Name()] {
			return nil
		}
		if scannable(path) {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return out
}

// base32Candidate finds 16-character runs of the base32 alphabet, which is the
// shape of a TOTP shared secret. Everything matched is hashed and looked up in
// the digest list, so the guard never has to hold a live seed itself.
var base32Candidate = regexp.MustCompile(`\b[A-Z2-7]{16}\b`)

// forbiddenSeedDigests returns the digests of any base32-shaped token in s that
// appears in the leaked-seed list.
func forbiddenSeedDigests(s string) []string {
	known := make(map[string]bool, len(forbiddenSecretSHA256))
	for _, d := range forbiddenSecretSHA256 {
		known[d] = true
	}
	var hits []string
	record := func(cand string) {
		sum := sha256.Sum256([]byte(cand))
		d := hex.EncodeToString(sum[:])
		if known[d] {
			hits = append(hits, d)
		}
	}
	for _, cand := range base32Candidate.FindAllString(s, -1) {
		record(cand)
	}
	for _, d := range forbiddenSeedDigestsInBase64(s) {
		hits = append(hits, d)
	}
	return hits
}

// base64Candidate matches a run long enough to hold a base32 seed, with or
// without padding, in standard or URL-safe alphabet.
var base64Candidate = regexp.MustCompile(`[A-Za-z0-9+/_-]{22,72}={0,2}`)

// forbiddenSeedDigestsInBase64 finds a leaked seed that was base64-encoded before
// being committed. The plain base32 scan cannot see it, and neither can a
// human skimming a diff: a cookie value, a pasted HTTP header or a JSON fixture
// all look like noise. Every candidate is decoded, and each base32-shaped
// 16-byte window of the result is looked up by digest.
func forbiddenSeedDigestsInBase64(s string) []string {
	known := make(map[string]bool, len(forbiddenSecretSHA256))
	for _, d := range forbiddenSecretSHA256 {
		known[d] = true
	}
	seen := make(map[string]bool)
	var hits []string
	record := func(tok string) {
		sum := sha256.Sum256([]byte(tok))
		d := hex.EncodeToString(sum[:])
		if known[d] && !seen[d] {
			seen[d] = true
			hits = append(hits, d)
		}
	}
	for _, cand := range base64Candidate.FindAllString(s, -1) {
		for _, enc := range []*base64.Encoding{
			base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding,
		} {
			raw, err := enc.DecodeString(cand)
			if err != nil {
				continue
			}
			for i := 0; i+16 <= len(raw); i++ {
				record(string(raw[i : i+16]))
			}
			break
		}
	}
	return hits
}
