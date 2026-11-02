package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// inputDirectories hold the files a scenario's commands hand to the cluster or
// print: the SQL, the migrations, the reference data and the manifests. A
// transcript that applies or prints one shows what the file held when it ran,
// so a file that changed is a scenario that changed, whatever the scenario
// file says.
//
// What is not here is deliberately not covered: demo/bin and demo/lib are the
// lab, which prepares a run and is not part of what a reader repeats, and
// demo/.lab is a run's own scratch.
var inputDirectories = []string{
	"demo/manifests",
	"demo/migrations",
	"demo/migrations-checkpoint",
	"demo/mysql",
	"demo/reference",
	"demo/schemas",
}

// inputMention is a repository path in a command. It starts where a word
// starts, so the demo in oci://registry/schemas/demo:v1 is not one.
var inputMention = regexp.MustCompile(`(^|[^A-Za-z0-9._/-])(demo/[A-Za-z0-9._/-]*)`)

// inputPaths names the files and directories under inputDirectories that a
// scenario's reset and steps mention, sorted and each once.
//
// A path that runs into a shell expansion, demo/mysql/release-$release, names
// files the shell chooses at run time, so the directory holding them stands
// for it.
func inputPaths(s scenario) []string {
	commands := slices.Clone(s.Reset)
	for _, current := range s.Steps {
		commands = append(commands, current.Run)
	}
	seen := map[string]bool{}
	for _, command := range commands {
		for _, match := range inputMention.FindAllStringSubmatchIndex(command, -1) {
			mentioned := command[match[4]:match[5]]
			if match[5] < len(command) && strings.ContainsRune("$*?[{", rune(command[match[5]])) {
				mentioned = path.Dir(mentioned)
			}
			mentioned = strings.TrimRight(mentioned, "/")
			if inInputDirectory(mentioned) {
				seen[mentioned] = true
			}
		}
	}
	paths := make([]string, 0, len(seen))
	for one := range seen {
		paths = append(paths, one)
	}
	slices.Sort(paths)
	return paths
}

func inInputDirectory(mentioned string) bool {
	for _, directory := range inputDirectories {
		if mentioned == directory || strings.HasPrefix(mentioned, directory+"/") {
			return true
		}
	}
	return false
}

// inputDigest is a digest of every file the scenario's commands mention, read
// under root. A path that does not exist is an error rather than a file left
// out: the command that names it would fail, and a digest that skipped it
// would bind the recording to less than it claims.
func inputDigest(root string, s scenario) (string, error) {
	hash := sha256.New()
	for _, mentioned := range inputPaths(s) {
		base := filepath.Join(root, filepath.FromSlash(mentioned))
		err := filepath.WalkDir(base, func(file string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			contents, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(root, file)
			if err != nil {
				return err
			}
			// The name and the length before the bytes, so two files cannot
			// run together into the digest of two others.
			fmt.Fprintf(hash, "%s\x00%d\x00", filepath.ToSlash(relative), len(contents))
			hash.Write(contents)
			return nil
		})
		if err != nil {
			return "", fmt.Errorf("%s reads %s: %w", s.ID, mentioned, err)
		}
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

// inputDigests is inputDigest for every scenario, by id.
func inputDigests(root string, scenarios []scenario) (map[string]string, error) {
	digests := make(map[string]string, len(scenarios))
	for _, one := range scenarios {
		digest, err := inputDigest(root, one)
		if err != nil {
			return nil, err
		}
		digests[one.ID] = digest
	}
	return digests, nil
}
