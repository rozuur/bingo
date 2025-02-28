// Copyright (c) Bartłomiej Płotka @bwplotka
// Licensed under the Apache License 2.0.

package main

import (
	"bufio"
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/bwplotka/bingo/pkg/bingo"
	"github.com/bwplotka/bingo/pkg/runner"
	"github.com/efficientgo/tools/core/pkg/errcapture"
	"github.com/pkg/errors"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

var (
	goModVersionRegexp = regexp.MustCompile("^v[0-9]*$")
)

func parseTarget(rawTarget string) (name string, pkgPath string, versions []string, err error) {
	if rawTarget == "" {
		return "", "", nil, errors.New("target is empty, this should be filtered earlier")
	}

	s := strings.Split(rawTarget, "@")
	nameOrPackage := s[0]
	if len(s) > 1 {
		versions = strings.Split(s[1], ",")
	} else {
		versions = []string{""}
	}

	if len(versions) > 1 {
		// Check for duplicates or/and none.
		dup := map[string]struct{}{}
		for _, v := range versions {
			if _, ok := dup[v]; ok {
				return "", "", nil, errors.Errorf("version duplicates are not allowed, got: %v", versions)
			}
			dup[v] = struct{}{}
			if v == "none" {
				return "", "", nil, errors.Errorf("none is not allowed when there are more than one specified Version, got: %v", versions)
			}
		}
	}

	name = nameOrPackage
	if strings.Contains(nameOrPackage, "/") {
		// Binary referenced by path, get default name from package path.
		pkgPath = nameOrPackage
		name = path.Base(pkgPath)
		if pkgSplit := strings.Split(pkgPath, "/"); len(pkgSplit) > 3 && goModVersionRegexp.MatchString(name) {
			// It's common pattern to name urls with versions in go modules. Exclude that.
			name = pkgSplit[len(pkgSplit)-2]
		}
	}
	return strings.ToLower(name), pkgPath, versions, nil
}

type installPackageConfig struct {
	runner    *runner.Runner
	modDir    string
	relModDir string
	update    runner.GetUpdatePolicy
	link      bool

	verbose bool
}

type getConfig struct {
	runner    *runner.Runner
	modDir    string
	relModDir string
	update    runner.GetUpdatePolicy
	name      string
	rename    string
	link      bool

	verbose bool
}

func (c getConfig) forPackage() installPackageConfig {
	return installPackageConfig{
		modDir:    c.modDir,
		relModDir: c.relModDir,
		runner:    c.runner,
		update:    c.update,
		verbose:   c.verbose,
		link:      c.link,
	}
}

func getAll(ctx context.Context, logger *log.Logger, c getConfig) (err error) {
	if c.name != "" {
		return errors.New("name cannot by specified if no target was given")
	}
	if c.rename != "" {
		return errors.New("rename cannot by specified if no target was given")
	}

	pkgs, err := bingo.ListPinnedMainPackages(logger, c.relModDir, false)
	if err != nil {
		return err
	}
	for _, p := range pkgs {
		for i, targetPkg := range p.ToPackages() {
			if err := getPackage(ctx, logger, c.forPackage(), i, p.Name, targetPkg); err != nil {
				return errors.Wrapf(err, "%d: getting %s", i, targetPkg.String())
			}
		}
	}
	return nil
}

func existingModFiles(modDir string, targetName string) (existingModFiles []string, _ error) {
	existingModFiles, err := filepath.Glob(filepath.Join(modDir, targetName+".mod"))
	if err != nil {
		return nil, err
	}
	existingModArrFiles, err := filepath.Glob(filepath.Join(modDir, targetName+".*.mod"))
	if err != nil {
		return nil, err
	}
	return append(existingModFiles, existingModArrFiles...), nil
}

// get performs bingo get: it's like go get, but package aware, without go source files and on dedicated mod file.
// rawTarget is name or target package path, optionally with module version or array versions.
func get(ctx context.Context, logger *log.Logger, c getConfig, rawTarget string) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute) // TODO(bwplotka): Put as param?
	defer cancel()

	// Cleanup all bingo modules' tmp files for fresh start.
	if err := cleanGoGetTmpFiles(c.modDir); err != nil {
		return err
	}
	if err := ensureModDirExists(logger, c.relModDir); err != nil {
		return errors.Wrap(err, "ensure mod dir")
	}

	if rawTarget == "" {
		// Empty target means to get all. It recursively invokes get for each existing binary.
		return getAll(ctx, logger, c)
	}

	// NOTE: pkgPath can be empty. This means that tool was referenced by name.
	name, pkgPath, versions, err := parseTarget(rawTarget)
	if err != nil {
		return errors.Wrapf(err, "parse %v", rawTarget)
	}

	if c.update != runner.NoUpdatePolicy {
		if versions[0] != "" || len(versions) > 1 {
			return errors.Errorf("-u specified; upgrade cannot take version arguments (string after @), got %v", versions)
		}
	}

	if c.rename != "" {
		// Treat rename specially.
		if pkgPath != "" {
			return errors.Errorf("-r rename has to reference installed tool by name not path, got: %v", pkgPath)
		}
		if versions[0] != "" || len(versions) > 1 {
			return errors.Errorf("-r rename cannot take version arguments (string after @), got %v", versions)
		}
		if err := validateNewName(versions, name, c.rename); err != nil {
			return errors.Wrap(err, "-r")
		}
		newExisting, err := existingModFiles(c.modDir, c.rename)
		if err != nil {
			return errors.Wrapf(err, "existing mod files for %v", c.rename)
		}
		if len(newExisting) > 0 {
			return errors.Errorf("found existing installed binaries %v under name you want to rename on. Remove target name %s or use different one", newExisting, c.rename)
		}

		existing, err := existingModFiles(c.modDir, name)
		if err != nil {
			return errors.Wrapf(err, "existing mod files for %v", name)
		}

		if len(existing) == 0 {
			return errors.Errorf("nothing to rename, tool %v not installed", name)
		}

		targets := make([]bingo.Package, 0, len(existing))
		for _, e := range existing {
			mf, err := bingo.OpenModFile(e)
			if err != nil {
				return errors.Wrapf(err, "found unparsable mod file %v. Uninstall it first via get %v@none or fix it manually.", e, name)
			}
			defer errcapture.Do(&err, mf.Close, "close")

			if mf.DirectPackage() == nil {
				return errors.Wrapf(err, "failed to rename tool %v to %v name; found empty mod file %v; Use full path to install tool again", name, c.rename, e)
			}
			targets = append(targets, *mf.DirectPackage())
		}

		for i, t := range targets {
			if err := getPackage(ctx, logger, c.forPackage(), i, c.rename, t); err != nil {
				return errors.Wrapf(err, "%s.mod: getting %s", c.rename, t)
			}
		}

		// Remove old mod files.
		return removeAllGlob(filepath.Join(c.modDir, name+".*"))
	}

	targetName := name
	if c.name != "" {
		if err := validateNewName(versions, name, c.name); err != nil {
			return errors.Wrap(err, "-n")
		}
		targetName = c.name
	}

	existing, err := existingModFiles(c.modDir, targetName)
	if err != nil {
		return errors.Wrapf(err, "existing mod files for %v", targetName)
	}

	switch versions[0] {
	case "none":
		if pkgPath != "" {
			return errors.Errorf("cannot delete tool by full path. Use just %v@none name instead", targetName)
		}
		if len(existing) == 0 {
			return errors.Errorf("nothing to delete, tool %v is not installed", targetName)
		}
		// None means we no longer want to version this package.
		// NOTE: We don't remove binaries.
		return removeAllGlob(filepath.Join(c.modDir, name+".*"))
	case "":
		if len(existing) > 1 && c.update == runner.NoUpdatePolicy {
			// Edge case. If no version is specified and no update is requested, allow to pull all array versions at once.
			versions = make([]string, len(existing))
		}
	}

	targets := make([]bingo.Package, 0, len(versions))
	pathWasSpecified := pkgPath != ""
	for i, v := range versions {
		target := bingo.Package{Module: module.Version{Version: v}, RelPath: pkgPath} // "Unknown" module mode.
		if len(existing) > i {
			e := existing[i]

			mf, err := bingo.OpenModFile(e)
			if err != nil {
				return errors.Wrapf(err, "found unparsable mod file %v. Uninstall it first via get %v@none or fix it manually.", e, name)
			}
			defer errcapture.Do(&err, mf.Close, "close")

			if mf.DirectPackage() != nil {
				if target.Path() != "" && target.Path() != mf.DirectPackage().Path() {
					if pathWasSpecified {
						return errors.Errorf("found mod file %v that has different package path %q than given %q"+
							"Uninstall existing tool using `%v@none` or use `-n` flag to choose different name", e, mf.DirectPackage().Path(), target.Path(), targetName)
					}
					return errors.Errorf("found array mod file %v that has different package path %q than previous in array %q. Manual edit?"+
						"Uninstall existing tool using `%v@none` or use `-n` flag to choose different name", e, mf.DirectPackage().Path(), target.Path(), targetName)
				}

				target.Module.Path = mf.DirectPackage().Module.Path
				if target.Module.Version == "" && c.update == runner.NoUpdatePolicy {
					// If no version and no update is requested, use the existing version.
					target.Module.Version = mf.DirectPackage().Module.Version
				}
				target.RelPath = mf.DirectPackage().RelPath

				// Save for future versions without potentially existing files.
				pkgPath = target.Path()
			} else if target.Path() == "" {
				return errors.Wrapf(err, "failed to install tool %v found empty mod file %v; Use full path to install tool again", targetName, e)
			}
		}
		if target.Path() == "" {
			return errors.Errorf("tool referenced by name %v that was never installed before; Use full path to install a tool", name)
		}
		targets = append(targets, target)
	}

	for i, t := range targets {
		if err := getPackage(ctx, logger, c.forPackage(), i, targetName, t); err != nil {
			return errors.Wrapf(err, "%s.mod: getting %s", targetName, t)
		}
	}

	// Remove target unused arr mod files based on version file.
	existingTargetModArrFiles, gerr := filepath.Glob(filepath.Join(c.modDir, targetName+".*.mod"))
	if gerr != nil {
		err = gerr
		return
	}
	for _, f := range existingTargetModArrFiles {
		i, perr := strconv.ParseInt(strings.Split(filepath.Base(f), ".")[1], 10, 64)
		if perr != nil || int(i) >= len(versions) {
			if rerr := os.RemoveAll(f); rerr != nil {
				err = rerr
				return
			}
		}
	}
	return nil
}

func validateNewName(versions []string, old, new string) error {
	if new == old {
		return errors.Errorf("cannot be the same as module name %v", new)
	}
	if versions[0] == "none" {
		return errors.Errorf("cannot use with @none logic")
	}
	return nil
}

func cleanGoGetTmpFiles(modDir string) error {
	// Remove all sum and tmp files
	if err := removeAllGlob(filepath.Join(modDir, "*.sum")); err != nil {
		return err
	}
	if err := removeAllGlob(filepath.Join(modDir, "*.*.tmp.*")); err != nil {
		return err
	}
	return removeAllGlob(filepath.Join(modDir, "*.tmp.*"))
}

func validateTargetName(targetName string) error {
	if targetName == "cmd" {
		return errors.Errorf("package would be installed with ambiguous name %s. This is a common, but slightly annoying package layout"+
			"It's advised to choose unique name with -n flag", targetName)
	}
	if targetName == strings.TrimSuffix(bingo.FakeRootModFileName, ".mod") {
		return errors.Errorf("requested binary with name %q`. This is impossible, choose different name using -n flag", strings.TrimSuffix(bingo.FakeRootModFileName, ".mod"))
	}
	return nil
}

func resolvePackage(
	logger *log.Logger,
	verbose bool,
	tmpModFile string,
	runnable runner.Runnable,
	update runner.GetUpdatePolicy,
	target *bingo.Package,
) (err error) {
	// Do initial go get -d and remember output.
	// NOTE: We have to use get -d to resolve version and tell us what is the module and what package.
	// If go get will not succeed, or will not update go mod, we will try manual lookup.
	// This is required to support modules depending on broken modules (and using exclude/replace statements).
	out, gerr := runnable.GetD(update, target.String())
	if gerr == nil {
		mods, err := bingo.ModIndirectModules(tmpModFile)
		if err != nil {
			return err
		}

		switch len(mods) {
		case 0:
			return errors.Errorf("no indirect module found on %v", tmpModFile)
		case 1:
			target.RelPath = strings.TrimPrefix(strings.TrimPrefix(target.RelPath, mods[0].Path), "/")
			target.Module = mods[0]
			return nil
		default:
			if target.Module.Path != "" {
				for _, m := range mods {
					if m.Path == target.Module.Path {
						target.RelPath = strings.TrimPrefix(strings.TrimPrefix(target.RelPath, m.Path), "/")
						target.Module = m
						return nil
					}
				}
				return errors.Errorf("no indirect module found on %v for %v module", tmpModFile, target.Module.Path)
			}

			for _, m := range mods {
				if m.Path == target.Path() {
					target.RelPath = strings.TrimPrefix(strings.TrimPrefix(target.RelPath, m.Path), "/")
					target.Module = m
					return nil
				}
			}

			// In this case it is not successful from our perspective.
			gerr = errors.New(out)
		}
	}

	// We fallback only if go-get failed which happens when it does not know what version to choose.
	// In this case
	if err := resolveInGoModCache(logger, verbose, update, target); err != nil {
		return errors.Wrapf(err, "fallback to local go mod cache resolution failed after go get failure: %v", gerr)
	}
	return nil
}

func gomodcache() string {
	cachepath := os.Getenv("GOMODCACHE")
	if gpath := os.Getenv("GOPATH"); gpath != "" && cachepath == "" {
		cachepath = filepath.Join(gpath, "pkg/mod")
	}
	return cachepath
}

func latestModVersion(listFile string) (_ string, err error) {
	f, err := os.Open(listFile)
	if err != nil {
		return "", err
	}
	defer errcapture.Do(&err, f.Close, "list file close")

	scanner := bufio.NewScanner(f)
	var lastVersion string
	for scanner.Scan() {
		lastVersion = scanner.Text()
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if lastVersion == "" {
		return "", errors.New("empty file")
	}
	return lastVersion, nil
}

// resolveInGoModCache will try to find a referenced module in the Go modules cache.
func resolveInGoModCache(logger *log.Logger, verbose bool, update runner.GetUpdatePolicy, target *bingo.Package) error {
	modMetaCache := filepath.Join(gomodcache(), "cache/download")
	modulePath := target.Path()

	// Since we don't know which part of full path is package, which part is module.
	// Start from longest and go until we find one.
	for ; len(strings.Split(modulePath, "/")) > 2; modulePath = filepath.Dir(modulePath) {
		modMetaDir := filepath.Join(modMetaCache, modulePath, "@v")
		if _, err := os.Stat(modMetaDir); err != nil {
			if os.IsNotExist(err) {
				if verbose {
					logger.Println("resolveInGoModCache:", modMetaDir, "directory does not exists")
				}
				continue
			}
			return err
		}
		if verbose {
			logger.Println("resolveInGoModCache: Found", modMetaDir, "directory")
		}

		// There are 2 major cases:
		// 1. We have -u flag or version is not pinned: find latest module having this package.
		if update != runner.NoUpdatePolicy || target.Module.Version == "" {
			latest, err := latestModVersion(filepath.Join(modMetaDir, "list"))
			if err != nil {
				return errors.Wrapf(err, "get latest version from %v", filepath.Join(modMetaDir, "list"))
			}

			target.Module.Path = modulePath
			target.Module.Version = latest
			target.RelPath = strings.TrimPrefix(strings.TrimPrefix(target.RelPath, target.Module.Path), "/")
			return nil
		}

		// 2. We don't have update flag and have version pinned: find exact version then.
		// Look for .info files that have exact version or sha.
		if strings.HasPrefix(target.Module.Version, "v") {
			if _, err := os.Stat(filepath.Join(modMetaDir, target.Module.Version+".info")); err != nil {
				if os.IsNotExist(err) {
					if verbose {
						logger.Println("resolveInGoModCache:", filepath.Join(modMetaDir, target.Module.Version+".info"),
							"file not exists. Looking for different module")
					}
					continue
				}
				return err
			}
			target.Module.Path = modulePath
			target.RelPath = strings.TrimPrefix(strings.TrimPrefix(target.RelPath, target.Module.Path), "/")
			return nil
		}

		// We have commit sha.
		files, err := ioutil.ReadDir(modMetaDir)
		if err != nil {
			return err
		}

		for _, f := range files {
			if f.IsDir() {
				continue
			}
			if len(target.Module.Version) > 12 && strings.HasSuffix(f.Name(), fmt.Sprintf("%v.info", target.Module.Version[:12])) {
				target.Module.Path = modulePath
				target.Module.Version = strings.TrimSuffix(f.Name(), ".info")
				target.RelPath = strings.TrimPrefix(strings.TrimPrefix(target.RelPath, target.Module.Path), "/")
				return nil
			}
		}

		if verbose {
			logger.Println("resolveInGoModCache: .info file for sha", target.Module.Version[:12],
				"does not exists. Looking for different module")
		}
	}
	return errors.Errorf("no module was cached matching given package %v", target.Path())
}

// getPackage takes package array index, tool name and package path (also module path and version which are optional) and
// generates new module with the given package's module as the only dependency (direct require statement).
// For generation purposes we take the existing <name>.mod file (if exists, if paths matches). This allows:
//  * Comments to be preserved.
//  * First direct require module will be preserved (unless version changes)
//  * Replace to be preserved if the // bingo:no_replace_fetch commend is found it such mod file.
// As resolution of module vs package for Go Module is convoluted and all code is under internal dir, we have to rely on `go` binary
// capabilities and output.
// TODO(bwplotka): Consider copying code for it? Of course it's would be easier if such tool would exist in Go project itself (:
func getPackage(ctx context.Context, logger *log.Logger, c installPackageConfig, i int, name string, target bingo.Package) (err error) {
	if c.verbose {
		logger.Println("getting target", target.String(), "(module", target.Module.Path, ")")
	}

	// The out module file we generate/maintain keep in modDir.
	outModFile := filepath.Join(c.modDir, name+".mod")
	tmpEmptyModFilePath := filepath.Join(c.modDir, name+"-e.tmp.mod")
	tmpModFilePath := filepath.Join(c.modDir, name+".tmp.mod")
	if i > 0 {
		// Handle array go modules.
		outModFile = filepath.Join(c.modDir, fmt.Sprintf("%s.%d.mod", name, i))
		tmpEmptyModFilePath = filepath.Join(c.modDir, fmt.Sprintf("%s.%d-e.tmp.mod", name, i))
		tmpModFilePath = filepath.Join(c.modDir, fmt.Sprintf("%s.%d.tmp.mod", name, i))
	}

	// If we don't have all information or update is set, resolve version.
	var replaceStmts []*modfile.Replace
	if target.Module.Version == "" || !strings.HasPrefix(target.Module.Version, "v") || target.Module.Path == "" || c.update != runner.NoUpdatePolicy {
		// Set up totally empty mod file to get clear version to install.
		tmpEmptyModFile, err := bingo.CreateFromExistingOrNew(ctx, c.runner, logger, "", tmpEmptyModFilePath)
		if err != nil {
			return errors.Wrap(err, "create empty tmp mod file")
		}
		defer errcapture.Do(&err, tmpEmptyModFile.Close, "close")

		runnable := c.runner.With(ctx, tmpEmptyModFile.FileName(), c.modDir, nil)
		if err := resolvePackage(logger, c.verbose, tmpEmptyModFile.FileName(), runnable, c.update, &target); err != nil {
			return err
		}

		if !strings.HasSuffix(target.Module.Version, "+incompatible") {
			replaceStmts, err = autoFetchReplaceStatements(runnable, target)
			if err != nil {
				return err
			}
		}
	}

	// Now we should have target with all required info, prepare tmp file.
	if err := cleanGoGetTmpFiles(c.modDir); err != nil {
		return err
	}
	tmpModFile, err := bingo.CreateFromExistingOrNew(ctx, c.runner, logger, outModFile, tmpModFilePath)
	if err != nil {
		return errors.Wrap(err, "create tmp mod file")
	}
	defer errcapture.Do(&err, tmpModFile.Close, "close")

	if !tmpModFile.AutoReplaceDisabled() && len(replaceStmts) > 0 {
		if err := tmpModFile.SetReplace(replaceStmts...); err != nil {
			return err
		}
	}

	// Currently user can't specify build flags and envvars from CLI, take if from optionally, manually updated mod file.
	if old := tmpModFile.DirectPackage(); old != nil {
		target.BuildEnvs = old.BuildEnvs
		target.BuildFlags = old.BuildFlags
	}
	if err := tmpModFile.SetDirectRequire(target); err != nil {
		return err
	}

	if err := tmpModFile.Flush(); err != nil {
		return err
	}

	if err := install(ctx, c.runner, c.modDir, name, c.link, tmpModFile); err != nil {
		return errors.Wrap(err, "install")
	}

	// We were working on tmp file, do atomic rename.
	if err := os.Rename(tmpModFile.FileName(), outModFile); err != nil {
		return errors.Wrap(err, "rename")
	}
	return nil
}

func localGoModFileAfterGet(gopath string, target bingo.Package) string {
	modulePath := target.Module.String()

	// Go get uses special notation for non-supported names. See https://github.com/bwplotka/bingo/issues/65.
	var b strings.Builder
	b.Grow(len(modulePath))
	for i := 0; i < len(modulePath); i++ {
		c := rune(modulePath[i])
		if 'A' <= c && c <= 'Z' {
			b.WriteByte('!')
			c = unicode.To(unicode.LowerCase, c)
		}
		b.WriteRune(c)
	}
	return filepath.Join(gopath, "pkg", "mod", b.String(), "go.mod")
}

// autoFetchReplaceStatements is reproducing replace statements to be exactly the same as the target module we want to install.
// It's a very common case where modules mitigate faulty modules or conflicts with replace directives.
// Since we always download single tool dependency module per tool module, we can copy its replace if exists to fix this common case.
func autoFetchReplaceStatements(runnable runner.Runnable, target bingo.Package) ([]*modfile.Replace, error) {
	gopath, err := runnable.GoEnv("GOPATH")
	if err != nil {
		return nil, errors.Wrap(err, "go env")
	}

	// We leverage fact that when go get runs if downloads the version we find as relevant locally
	// in the GOPATH/pkg/mod/...
	targetModFile := localGoModFileAfterGet(gopath, target)
	if _, err := os.Stat(targetModFile); err != nil {
		if os.IsNotExist(err) {
			// Pre module package.
			return nil, nil
		}
		return nil, errors.Wrapf(err, "stat target mod directory %v", targetModFile)
	}

	targetModParsed, err := bingo.ParseModFileOrReader(targetModFile, nil)
	if err != nil {
		return nil, errors.Wrapf(err, "parse target mod file %v", targetModFile)
	}
	return targetModParsed.Replace, nil
}

// gobin mimics the way go install finds where to install go tool.
func gobin() string {
	binPath := os.Getenv("GOBIN")
	if gpath := os.Getenv("GOPATH"); gpath != "" && binPath == "" {
		binPath = filepath.Join(gpath, "bin")
	}
	return binPath
}

func install(ctx context.Context, r *runner.Runner, modDir string, name string, link bool, modFile *bingo.ModFile) (err error) {
	pkg := modFile.DirectPackage()
	if err := validateTargetName(name); err != nil {
		return errors.Wrap(err, pkg.String())
	}

	// Two purposes of doing list with mod=mod:
	// * Check if path is pointing to non-buildable package.
	// * Rebuild go.sum and go.mod (tidy) which is required to build with -mod=readonly (default) to work.
	var listArgs []string
	listArgs = append(listArgs, modFile.DirectPackage().BuildFlags...)
	listArgs = append(listArgs, "-mod=mod", "-f={{.Name}}", pkg.Path())
	if listOutput, err := r.With(ctx, modFile.FileName(), modDir, nil).List(runner.NoUpdatePolicy, listArgs...); err != nil {
		return errors.Wrap(err, "list")
	} else if !strings.HasSuffix(listOutput, "main") {
		return errors.Errorf("package %s is non-main (go list output %q), nothing to get and build", pkg.Path(), listOutput)
	}

	gobin := gobin()

	// go install does not define -modfile flag so so we mimic go install with go build -o instead.
	binPath := filepath.Join(gobin, fmt.Sprintf("%s-%s", name, pkg.Module.Version))
	if err := r.With(ctx, modFile.FileName(), modDir, pkg.BuildEnvs).Build(pkg.Path(), binPath, pkg.BuildFlags...); err != nil {
		return errors.Wrap(err, "build versioned")
	}

	if !link {
		return nil
	}

	if err := os.RemoveAll(filepath.Join(gobin, name)); err != nil {
		return errors.Wrap(err, "rm")
	}
	if err := os.Symlink(binPath, filepath.Join(gobin, name)); err != nil {
		return errors.Wrap(err, "symlink")
	}
	return nil
}

const modREADMEFmt = `# Project Development Dependencies.

This is directory which stores Go modules with pinned buildable package that is used within this repository, managed by https://github.com/bwplotka/bingo.

* Run ` + "`" + "bingo get" + "`" + ` to install all tools having each own module file in this directory.
* Run ` + "`" + "bingo get <tool>" + "`" + ` to install <tool> that have own module file in this directory.
* For Makefile: Make sure to put ` + "`" + "include %s/Variables.mk" + "`" + ` in your Makefile, then use $(<upper case tool name>) variable where <tool> is the %s/<tool>.mod.
* For shell: Run ` + "`" + "source %s/variables.env" + "`" + ` to source all environment variable for each tool.
* For go: Import ` + "`" + "%s/variables.go" + "`" + ` to for variable names.
* See https://github.com/bwplotka/bingo or -h on how to add, remove or change binaries dependencies.

## Requirements

* Go 1.14+
`

const gitignore = `
# Ignore everything
*

# But not these files:
!.gitignore
!*.mod
!README.md
!Variables.mk
!variables.env

*tmp.mod
`

func ensureModDirExists(logger *log.Logger, relModDir string) error {
	_, err := os.Stat(relModDir)
	if err != nil {
		if !os.IsNotExist(err) {
			return errors.Wrapf(err, "stat bingo module dir %s", relModDir)
		}

		logger.Printf("Bingo not used before here, creating directory for pinned modules for you at %s\n", relModDir)
		if err := os.MkdirAll(relModDir, os.ModePerm); err != nil {
			return errors.Wrapf(err, "create moddir %s", relModDir)
		}
	}

	// Hack against:
	// "A file named go.mod must still be present in order to determine the module root directory, but it is not accessed."
	// Ref: https://golang.org/doc/go1.14#go-flags
	// TODO(bwplotka): Remove it: https://github.com/bwplotka/bingo/issues/20
	if err := ioutil.WriteFile(
		filepath.Join(relModDir, bingo.FakeRootModFileName),
		[]byte("module _ // Fake go.mod auto-created by 'bingo' for go -moddir compatibility with non-Go projects. Commit this file, together with other .mod files."),
		0666,
	); err != nil {
		return err
	}

	// README.
	if err := ioutil.WriteFile(
		filepath.Join(relModDir, "README.md"),
		[]byte(fmt.Sprintf(modREADMEFmt, relModDir, relModDir, relModDir, relModDir)),
		0666,
	); err != nil {
		return err
	}
	// gitignore.
	return ioutil.WriteFile(filepath.Join(relModDir, ".gitignore"), []byte(gitignore), 0666)
}

func removeAllGlob(glob string) error {
	files, err := filepath.Glob(glob)
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := os.RemoveAll(f); err != nil {
			return err
		}
	}
	return nil
}
