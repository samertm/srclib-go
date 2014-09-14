package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"

	"go/build"

	"github.com/golang/gddo/gosrc"

	"strings"
	"sync"

	"sourcegraph.com/sourcegraph/srclib/dep"
	"sourcegraph.com/sourcegraph/srclib/unit"
)

func init() {
	_, err := parser.AddCommand("depresolve",
		"resolve a Go package's imports",
		"Resolve a Go package's imports to their repository clone URL.",
		&depResolveCmd,
	)
	if err != nil {
		log.Fatal(err)
	}
}

type DepResolveCmd struct {
	Config []string `long:"config" description:"config property from Srcfile" value-name:"KEY=VALUE"`
}

var depResolveCmd DepResolveCmd

func (c *DepResolveCmd) Execute(args []string) error {
	var unit *unit.SourceUnit
	if err := json.NewDecoder(os.Stdin).Decode(&unit); err != nil {
		return err
	}
	if err := os.Stdin.Close(); err != nil {
		return err
	}

	if err := unmarshalTypedConfig(unit.Config); err != nil {
		return err
	}
	if err := config.apply(); err != nil {
		return err
	}

	res := make([]*dep.Resolution, len(unit.Dependencies))
	for i, rawDep := range unit.Dependencies {
		importPath, ok := rawDep.(string)
		if !ok {
			return fmt.Errorf("Go raw dep is not a string import path: %v (%T)", rawDep, rawDep)
		}

		res[i] = &dep.Resolution{Raw: rawDep}

		rt, err := ResolveDep(importPath, string(unit.Repo))
		if err != nil {
			res[i].Error = err.Error()
			continue
		}
		res[i].Target = rt
	}

	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	if _, err := os.Stdout.Write(b); err != nil {
		return err
	}
	return nil
}

var (
	resolveCache   map[string]*dep.ResolvedTarget
	resolveCacheMu sync.Mutex
)

func ResolveDep(importPath string, repoImportPath string) (*dep.ResolvedTarget, error) {
	// Look up in cache.
	resolvedTarget := func() *dep.ResolvedTarget {
		resolveCacheMu.Lock()
		defer resolveCacheMu.Unlock()
		return resolveCache[importPath]
	}()
	if resolvedTarget != nil {
		return resolvedTarget, nil
	}
	if strings.HasSuffix(importPath, "_test") {
		// TODO(sqs): handle xtest packages - these should not be appearing here
		// as import paths, but they are, so suppress errors
		return nil, fmt.Errorf("xtest package (%s) is not yet supported", importPath)
	}

	// Check if this import path is in this tree.
	if pkg, err := buildContext.Import(importPath, "", build.FindOnly); err == nil && (pathHasPrefix(pkg.Dir, cwd) || (virtualCWD != "" && pathHasPrefix(pkg.Dir, virtualCWD)) || (dockerCWD != "" && pathHasPrefix(pkg.Dir, dockerCWD))) {
		return &dep.ResolvedTarget{
			// empty ToRepoCloneURL to indicate it's from this repository
			ToRepoCloneURL: "",
			ToUnit:         importPath,
			ToUnitType:     "GoPackage",
		}, nil
	}

	// Check if this import path is in this repository.
	if strings.HasPrefix(importPath, repoImportPath) {
		return &dep.ResolvedTarget{
			// empty ToRepoCloneURL to indicate it's from this repository
			ToRepoCloneURL: "",
			ToUnit:         importPath,
			ToUnitType:     "GoPackage",
		}, nil
	}

	// Special-case the cgo package "C".
	if importPath == "C" {
		return nil, nil
	}

	if gosrc.IsGoRepoPath(importPath) || importPath == "debug/goobj" || importPath == "debug/plan9obj" {
		return &dep.ResolvedTarget{
			ToRepoCloneURL:  "https://code.google.com/p/go",
			ToVersionString: runtime.Version(),
			ToRevSpec:       "", // TODO(sqs): fill in when graphing stdlib repo
			ToUnit:          importPath,
			ToUnitType:      "GoPackage",
		}, nil
	}

	// Special-case github.com/... import paths for performance.
	if strings.HasPrefix(importPath, "github.com/") || strings.HasPrefix(importPath, "sourcegraph.com/") {
		parts := strings.SplitN(importPath, "/", 4)
		if len(parts) < 3 {
			return nil, fmt.Errorf("import path starts with '(github|sourcegraph).com/' but is not valid: %q", importPath)
		}
		return &dep.ResolvedTarget{
			ToRepoCloneURL: "https://" + strings.Join(parts[:3], "/") + ".git",
			ToUnit:         importPath,
			ToUnitType:     "GoPackage",
		}, nil
	}

	// Special-case code.google.com/p/... import paths for performance.
	if strings.HasPrefix(importPath, "code.google.com/p/") {
		parts := strings.SplitN(importPath, "/", 4)
		if len(parts) < 3 {
			return nil, fmt.Errorf("import path starts with 'code.google.com/p/' but is not valid: %q", importPath)
		}
		return &dep.ResolvedTarget{
			ToRepoCloneURL: "https://" + strings.Join(parts[:3], "/"),
			ToUnit:         importPath,
			ToUnitType:     "GoPackage",
		}, nil
	}

	log.Printf("Resolving Go dep: %s", importPath)

	dir, err := gosrc.Get(http.DefaultClient, string(importPath), "")
	if err != nil {
		if strings.Contains(err.Error(), "Git Repository is empty.") {
			// Not fatal, just weird.
			return nil, nil
		}
		return nil, fmt.Errorf("unable to fetch information about Go package %q: %s", importPath, err)
	}

	// gosrc returns code.google.com URLs ending in a slash. Remove it.
	dir.ProjectURL = strings.TrimSuffix(dir.ProjectURL, "/")

	resolvedTarget = &dep.ResolvedTarget{
		ToRepoCloneURL: dir.ProjectURL,
		ToUnit:         importPath,
		ToUnitType:     "GoPackage",
	}

	// Save in cache.
	resolveCacheMu.Lock()
	defer resolveCacheMu.Unlock()
	if resolveCache == nil {
		resolveCache = make(map[string]*dep.ResolvedTarget)
	}
	resolveCache[importPath] = resolvedTarget

	return resolvedTarget, nil
}
