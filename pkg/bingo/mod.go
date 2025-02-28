// Copyright (c) Bartłomiej Płotka @bwplotka
// Licensed under the Apache License 2.0.

package bingo

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/bwplotka/bingo/pkg/envars"
	"github.com/bwplotka/bingo/pkg/runner"
	"github.com/efficientgo/tools/core/pkg/errcapture"
	"github.com/efficientgo/tools/core/pkg/merrors"
	"github.com/pkg/errors"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

const (
	// FakeRootModFileName is a name for fake go module that we have to maintain, until https://github.com/bwplotka/bingo/issues/20 is fixed.
	FakeRootModFileName = "go.mod"

	NoReplaceCommand = "bingo:no_replace_fetch"

	PackageRenderablesPrintHeader = "Name\tBinary Name\tPackage @ Version\tBuild EnvVars\tBuild Flags\n" +
		"----\t-----------\t-----------------\t-------------\t-----------\n"
)

// NameFromModFile returns binary name from module file path.
func NameFromModFile(modFile string) (name string, oneOfMany bool) {
	n := strings.Split(strings.TrimSuffix(filepath.Base(modFile), ".mod"), ".")
	if len(n) > 1 {
		oneOfMany = true
	}
	return n[0], oneOfMany
}

// A Package (for clients, a bingo.Package) is defined by a module path, package relative path and version pair.
// These are stored in their plain (unescaped) form.
type Package struct {
	Module module.Version

	// RelPath is a path that together with Module.Path composes a package path, like "/pkg/makefile".
	// Empty if the module is a full package path.
	// If Module.Path is empty and RelPath specified, it means that we don't know what is a module what is the package path.
	RelPath string

	// BuildEnvs are environment variables to be used during go build process.
	BuildEnvs envars.EnvSlice
	// BuildFlags are flags to be used during go build process.
	BuildFlags []string
}

// String returns a representation of the Package suitable for `go` tools and logging.
// (Module.Path/RelPath@Module.Version, or Module.Path/RelPath if Version is empty).
func (m Package) String() string {
	if m.Module.Version == "" {
		return m.Path()
	}
	return m.Path() + "@" + m.Module.Version
}

// Path returns a full package path.
func (m Package) Path() string {
	return filepath.Join(m.Module.Path, m.RelPath)
}

// ModFile represents bingo tool .mod file.
type ModFile struct {
	filename string

	f *os.File
	m *modfile.File

	directPackage       *Package
	autoReplaceDisabled bool
}

// OpenModFile opens bingo mod file.
// It also adds meta if missing and trims all require direct module imports except first within the parsed syntax.
// It's a caller responsibility to Close the file when not using anymore.
func OpenModFile(modFile string) (_ *ModFile, err error) {
	f, err := os.OpenFile(modFile, os.O_RDWR, os.ModePerm)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			errcapture.Do(&err, f.Close, "close")
		}
	}()
	mf := &ModFile{f: f, filename: modFile}
	if err := mf.Reload(); err != nil {
		return nil, err
	}

	if err := onModHeaderComments(mf.m, func(comments *modfile.Comments) error {
		if err := errOnMetaMissing(comments); err != nil {
			mf.m.Module.Syntax.Suffix = append(mf.m.Module.Syntax.Suffix, modfile.Comment{Suffix: true, Token: metaComment})
			return nil
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return mf, nil
}

// CreateFromExistingOrNew creates and opens new bingo enhanced module file.
// If existing file exists and is not malformed it copies this as the source, otherwise completely new is created.
// It's a caller responsibility to Close the file when not using anymore.
func CreateFromExistingOrNew(ctx context.Context, r *runner.Runner, logger *log.Logger, existingFile, modFile string) (*ModFile, error) {
	if err := os.RemoveAll(modFile); err != nil {
		return nil, errors.Wrap(err, "rm")
	}

	if existingFile != "" {
		_, err := os.Stat(existingFile)
		if err != nil && !os.IsNotExist(err) {
			return nil, errors.Wrapf(err, "stat module file %s", existingFile)
		}
		if err == nil {
			// Only use existing mod file on successful parse.
			o, err := OpenModFile(existingFile)
			if err == nil {
				if err := o.Close(); err != nil {
					return nil, err
				}
				if err := copyFile(existingFile, modFile); err != nil {
					return nil, err
				}
				return OpenModFile(modFile)
			}
			logger.Printf("bingo tool module file %v is malformed; it will be recreated; err: %v\n", existingFile, err)
		}
	}

	// Create from scratch.
	if err := r.ModInit(ctx, filepath.Dir(existingFile), modFile, "_"); err != nil {
		return nil, errors.Wrap(err, "mod init")
	}
	return OpenModFile(modFile)
}

func copyFile(src, dst string) error {
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	// TODO(bwplotka): Check those errors in defer.
	defer source.Close()
	destination, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destination.Close()

	buf := make([]byte, 1024)
	for {
		n, err := source.Read(buf)
		if err != nil && err != io.EOF {
			return err
		}
		if n == 0 {
			break
		}

		if _, err := destination.Write(buf[:n]); err != nil {
			return err
		}
	}
	return nil
}

func (mf *ModFile) FileName() string {
	return mf.filename
}

func (mf *ModFile) AutoReplaceDisabled() bool {
	return mf.autoReplaceDisabled
}

// Close flushes changes and closes file.
func (mf *ModFile) Close() error {
	return merrors.New(mf.Flush(), mf.f.Close()).Err()
}

func (mf *ModFile) Reload() (err error) {
	if _, err := mf.f.Seek(0, 0); err != nil {
		return errors.Wrap(err, "seek")
	}

	mf.m, err = ParseModFileOrReader(mf.filename, mf.f)
	if err != nil {
		return err
	}

	mf.autoReplaceDisabled = false
	for _, e := range mf.m.Syntax.Stmt {
		for _, c := range e.Comment().Before {
			if strings.Contains(c.Token, NoReplaceCommand) {
				mf.autoReplaceDisabled = true
				break
			}
		}
		for _, c := range e.Comment().After {
			if strings.Contains(c.Token, NoReplaceCommand) {
				mf.autoReplaceDisabled = true
				break
			}
		}
		for _, c := range e.Comment().Suffix {
			if strings.Contains(c.Token, NoReplaceCommand) {
				mf.autoReplaceDisabled = true
				break
			}
		}
	}

	// We expect just one direct import if any.
	mf.directPackage = nil
	for _, r := range mf.m.Require {
		if r.Indirect {
			continue
		}

		mf.directPackage = &Package{Module: r.Mod}
		if len(r.Syntax.Suffix) > 0 {
			mf.directPackage.RelPath, mf.directPackage.BuildEnvs, mf.directPackage.BuildFlags = parseDirectPackageMeta(strings.Trim(r.Syntax.Suffix[0].Token[3:], "\n"))
		}
		break
	}
	// Remove rest.
	mf.dropAllRequire()
	if mf.directPackage != nil {
		return mf.SetDirectRequire(*mf.directPackage)
	}
	return nil
}

func parseDirectPackageMeta(line string) (relPath string, buildEnv []string, buildFlags []string) {
	elem := strings.Split(line, " ")
	for i, l := range elem {
		if l == "" {
			continue
		}

		if l[0] == '-' {
			buildFlags = elem[i:]
			break
		}

		if !strings.Contains(l, "=") {
			relPath = l
			continue
		}
		buildEnv = append(buildEnv, l)
	}
	return relPath, buildEnv, buildFlags
}

func (mf *ModFile) DirectPackage() *Package {
	return mf.directPackage
}

// Flush saves all changes made to parsed syntax and reloads the parsed file.
func (mf *ModFile) Flush() error {
	newB := modfile.Format(mf.m.Syntax)
	if err := mf.f.Truncate(0); err != nil {
		return errors.Wrap(err, "truncate")
	}
	if _, err := mf.f.Seek(0, 0); err != nil {
		return errors.Wrap(err, "seek")
	}
	if _, err := mf.f.Write(newB); err != nil {
		return errors.Wrap(err, "write")
	}
	return mf.Reload()
}

// SetDirectRequire removes all require statements and set to the given one. It supports package level versioning.
// It's caller responsibility to Flush all changes.
func (mf *ModFile) SetDirectRequire(target Package) (err error) {
	mf.dropAllRequire()
	mf.m.AddNewRequire(target.Module.Path, target.Module.Version, false)

	var meta []string
	// Add sub package info if needed.
	if target.RelPath != "" && target.RelPath != "." {
		meta = append(meta, target.RelPath)
	}
	meta = append(meta, target.BuildEnvs...)
	meta = append(meta, target.BuildFlags...)

	if len(meta) > 0 {
		r := mf.m.Require[0]
		r.Syntax.Suffix = append(r.Syntax.Suffix[:0], modfile.Comment{Suffix: true, Token: "// " + strings.Join(meta, " ")})
	}

	mf.m.Cleanup()
	mf.directPackage = &target
	return nil
}

func (mf *ModFile) dropAllRequire() {
	for _, r := range mf.m.Require {
		if r.Syntax == nil {
			continue
		}
		_ = mf.m.DropRequire(r.Mod.Path)
	}
	mf.m.Require = mf.m.Require[:0]
}

// SetReplace removes all replace statements and set to the given ones.
// It's caller responsibility to Flush all changes.
func (mf *ModFile) SetReplace(target ...*modfile.Replace) (err error) {
	for _, r := range mf.m.Replace {
		if err := mf.m.DropReplace(r.Old.Path, r.Old.Version); err != nil {
			return err
		}
	}
	for _, r := range target {
		if err := mf.m.AddReplace(r.Old.Path, r.Old.Version, r.New.Path, r.New.Version); err != nil {
			return err
		}
	}
	mf.m.Cleanup()
	return nil
}

// ParseModFileOrReader parses any module file or reader allowing to read it's content.
func ParseModFileOrReader(modFile string, r io.Reader) (*modfile.File, error) {
	b, err := readAllFileOrReader(modFile, r)
	if err != nil {
		return nil, errors.Wrap(err, "read")
	}

	m, err := modfile.Parse(modFile, b, nil)
	if err != nil {
		return nil, errors.Wrap(err, "parse")
	}
	return m, nil
}

func readAllFileOrReader(file string, r io.Reader) (b []byte, err error) {
	if r != nil {
		return ioutil.ReadAll(r)
	}
	return ioutil.ReadFile(file)
}

// ModDirectPackage return the first direct package from bingo enhanced module file. The package suffix (if any) is
// encoded in the line comment, in the same line as module and version.
func ModDirectPackage(modFile string) (pkg Package, err error) {
	mf, err := OpenModFile(modFile)
	if err != nil {
		return Package{}, err
	}
	defer errcapture.Do(&err, mf.Close, "close")

	if mf.directPackage == nil {
		return Package{}, errors.Errorf("no direct package found in %s; empty module?", mf.filename)
	}
	return *mf.directPackage, nil
}

// ModIndirectModules return the all indirect mod from any module file.
func ModIndirectModules(modFile string) (mods []module.Version, err error) {
	m, err := ParseModFileOrReader(modFile, nil)
	if err != nil {
		return nil, err
	}

	for _, r := range m.Require {
		if !r.Indirect {
			continue
		}

		mods = append(mods, r.Mod)
	}
	return mods, nil
}

const metaComment = "// Auto generated by https://github.com/bwplotka/bingo. DO NOT EDIT"

func onModHeaderComments(m *modfile.File, f func(*modfile.Comments) error) error {
	if m.Module == nil {
		return errors.New("failed to parse; no module")
	}
	if m.Module.Syntax == nil {
		return errors.New("failed to parse; no module's syntax")
	}
	if m.Module.Syntax.Comment() == nil {
		return errors.Errorf("expected %q comment on top of module, found no comment", metaComment)
	}
	return f(m.Module.Syntax.Comment())
}

func errOnMetaMissing(comments *modfile.Comments) error {
	if len(comments.Suffix) == 0 {
		return errors.Errorf("expected %q comment on top of module, found no comment", metaComment)
	}

	tr := strings.Trim(comments.Suffix[0].Token, "\n")
	if tr != metaComment {
		return errors.Errorf("expected %q comment on top of module, found %q", metaComment, tr)
	}
	return nil
}

// PackageVersionRenderable is used in variables.go. Modify with care.
type PackageVersionRenderable struct {
	Version string
	ModFile string
}

// PackageRenderable is used in variables.go. Modify with care.
type PackageRenderable struct {
	Name        string
	ModPath     string
	PackagePath string
	EnvVarName  string
	Versions    []PackageVersionRenderable

	BuildFlags   []string
	BuildEnvVars []string
}

func (p PackageRenderable) ToPackages() []Package {
	ret := make([]Package, 0, len(p.Versions))
	for _, v := range p.Versions {
		relPath, _ := filepath.Rel(p.ModPath, p.PackagePath)

		ret = append(ret, Package{
			Module: module.Version{
				Version: v.Version,
				Path:    p.ModPath,
			},
			RelPath: relPath,
		})
	}
	return ret
}

type PackageRenderables []PackageRenderable

func (pkgs PackageRenderables) PrintTab(target string, w io.Writer) error {
	tw := new(tabwriter.Writer)
	tw.Init(w, 1, 8, 1, '\t', tabwriter.AlignRight)
	defer func() { _ = tw.Flush() }()

	_, _ = fmt.Fprint(tw, PackageRenderablesPrintHeader)
	for _, p := range pkgs {
		if target != "" && p.Name != target {
			continue
		}
		for _, v := range p.Versions {
			fields := []string{
				p.Name,
				p.Name + "-" + v.Version,
				p.PackagePath + "@" + v.Version,
				strings.Join(p.BuildEnvVars, " "),
				strings.Join(p.BuildFlags, " "),
			}
			_, _ = fmt.Fprintln(tw, strings.Join(fields, "\t"))
		}
		if target != "" {
			return nil
		}
	}

	if target != "" {
		return errors.Errorf("Pinned tool %s not found", target)
	}
	return nil
}

// ListPinnedMainPackages lists all bingo pinned binaries (Go main packages) in the same order as seen in the filesystem.
func ListPinnedMainPackages(logger *log.Logger, modDir string, remMalformed bool) (pkgs PackageRenderables, _ error) {
	modFiles, err := filepath.Glob(filepath.Join(modDir, "*.mod"))
	if err != nil {
		return nil, err
	}
ModLoop:
	for _, f := range modFiles {
		if filepath.Base(f) == FakeRootModFileName {
			continue
		}

		pkg, err := ModDirectPackage(f)
		if err != nil {
			if remMalformed {
				logger.Printf("found malformed module file %v, removing due to error: %v\n", f, err)
				if err := os.RemoveAll(strings.TrimSuffix(f, ".") + "*"); err != nil {
					return nil, err
				}
			}
			continue
		}

		name, _ := NameFromModFile(f)
		varName := strings.ReplaceAll(strings.ReplaceAll(strings.ToUpper(name), ".", "_"), "-", "_")
		for i, p := range pkgs {
			if p.Name == name {
				pkgs[i].EnvVarName = varName + "_ARRAY"
				// Preserve order. Unfortunately first array mod file has no number, so it's last.
				if filepath.Base(f) == p.Name+".mod" {
					pkgs[i].Versions = append([]PackageVersionRenderable{{
						Version: pkg.Module.Version,
						ModFile: filepath.Base(f),
					}}, pkgs[i].Versions...)
					continue ModLoop
				}

				pkgs[i].Versions = append(pkgs[i].Versions, PackageVersionRenderable{
					Version: pkg.Module.Version,
					ModFile: filepath.Base(f),
				})
				continue ModLoop
			}
		}
		pkgs = append(pkgs, PackageRenderable{
			Name: name,
			Versions: []PackageVersionRenderable{
				{Version: pkg.Module.Version, ModFile: filepath.Base(f)},
			},
			BuildFlags:   pkg.BuildFlags,
			BuildEnvVars: pkg.BuildEnvs,

			EnvVarName:  varName,
			PackagePath: pkg.Path(),
			ModPath:     pkg.Module.Path,
		})
	}
	return pkgs, nil
}

func SortRenderables(pkgs []PackageRenderable) {
	for _, p := range pkgs {
		sort.Slice(p.Versions, func(i, j int) bool {
			return p.Versions[i].Version < p.Versions[j].Version
		})
	}
	sort.Slice(pkgs, func(i, j int) bool {
		if pkgs[i].Name == pkgs[j].Name {
			return pkgs[i].PackagePath < pkgs[j].PackagePath
		}
		return pkgs[i].Name < pkgs[j].Name
	})
}
