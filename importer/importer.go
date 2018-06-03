// NOTE: most of this file is copied from "go/internal/srcimporter"

package importer

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"
	"sync"
)

var (
	// Importing is a sentinel taking the place in cachedPkgs
	// for a package that is in the process of being imported.
	importing  types.Package
	cachedPkgs sync.Map
)

type Importer struct {
	ctxt  *build.Context
	fset  *token.FileSet
	sizes types.Sizes
}

func New(ctx *PackedContext, filename string) types.ImporterFrom {
	ctxt := &build.Context{
		GOARCH:        ctx.GOARCH,
		GOOS:          ctx.GOOS,
		GOROOT:        ctx.GOROOT,
		GOPATH:        ctx.GOPATH,
		CgoEnabled:    ctx.CgoEnabled,
		UseAllFiles:   ctx.UseAllFiles,
		Compiler:      ctx.Compiler,
		BuildTags:     ctx.BuildTags,
		ReleaseTags:   ctx.ReleaseTags,
		InstallSuffix: ctx.InstallSuffix,
	}

	paths := filepath.SplitList(ctxt.GOPATH)
	slashed := filepath.ToSlash(filename)
	i := strings.LastIndex(slashed, "/vendor/src/")
	if i < 0 {
		i = strings.LastIndex(slashed, "/src/")
	}
	if i > 0 {
		gbroot := filepath.FromSlash(slashed[:i])
		gbvendor := filepath.Join(gbroot, "vendor")
		if gbroot == ctxt.GOROOT {
			goto Found
		}
		for _, path := range paths {
			if path == gbroot || path == gbvendor {
				goto Found
			}
		}

		paths = append(paths, gbroot, gbvendor)

		ctxt.SplitPathList = func(list string) []string {
			return paths
		}

		ctxt.JoinPath = func(elem ...string) string {
			match := func(s, prefix string) (string, bool) {
				rest := strings.TrimPrefix(s, prefix)
				return rest, len(rest) < len(s)
			}

			res := filepath.Join(elem...)
			// Want to rewrite "$GBROOT/(vendor/)?pkg/$GOOS_$GOARCH(_)?"
			// into "$GBROOT/pkg/$GOOS-$GOARCH(-)?".
			// Note: gb doesn't use vendor/pkg.
			if gbrel, err := filepath.Rel(gbroot, res); err == nil {
				gbrel = filepath.ToSlash(gbrel)
				gbrel, _ = match(gbrel, "vendor/")
				if gbrel, ok := match(gbrel, fmt.Sprintf("pkg/%s_%s", ctxt.GOOS, ctxt.GOARCH)); ok {
					gbrel, hasSuffix := match(gbrel, "_")

					// Reassemble into result.
					if hasSuffix {
						gbrel = "-" + gbrel
					}
					gbrel = fmt.Sprintf("pkg/%s-%s/", ctxt.GOOS, ctxt.GOARCH) + gbrel
					gbrel = filepath.FromSlash(gbrel)
					res = filepath.Join(gbroot, gbrel)
				}
			}

			return res
		}

	Found:
	}

	for _, path := range paths {
		if !strings.HasPrefix(filename, path) {
			continue
		}
		if rel, err := filepath.Rel(path, filename); err == nil {
			rel = filepath.ToSlash(rel)
			if i := strings.LastIndexByte(rel, '/'); i > 4 { // 4 to remove 'src/'
				fmt.Println("current package removed:", rel[4:i])
				// current package is being modified, remove it from cache
				cachedPkgs.Delete(rel[4:i])
			}
			break
		}
	}

	return &Importer{
		ctxt:  ctxt,
		fset:  token.NewFileSet(),
		sizes: types.SizesFor(ctxt.Compiler, ctxt.GOARCH), // uses go/types default if GOARCH not found
	}
}

// Import(path) is a shortcut for ImportFrom(path, "", 0).
func (p *Importer) Import(path string) (*types.Package, error) {
	return p.ImportFrom(path, "", 0)
}

// ImportFrom imports the package with the given import path resolved from the given srcDir,
// adds the new package to the set of packages maintained by the importer, and returns the
// package. Package path resolution and file system operations are controlled by the context
// maintained with the importer. The import mode must be zero but is otherwise ignored.
// Packages that are not comprised entirely of pure Go files may fail to import because the
// type checker may not be able to determine all exported entities (e.g. due to cgo dependencies).
func (p *Importer) ImportFrom(path, srcDir string, mode types.ImportMode) (*types.Package, error) {
	if mode != 0 {
		panic("non-zero import mode")
	}

	// determine package path (do vendor resolution)
	var bp *build.Package
	var err error
	switch {
	default:
		if abs, err := p.absPath(srcDir); err == nil { // see issue #14282
			srcDir = abs
		}
		bp, err = p.ctxt.Import(path, srcDir, build.FindOnly)

	case build.IsLocalImport(path):
		// "./x" -> "srcDir/x"
		bp, err = p.ctxt.ImportDir(filepath.Join(srcDir, path), build.FindOnly)

	case p.isAbsPath(path):
		return nil, fmt.Errorf("invalid absolute import path %q", path)
	}
	if err != nil {
		return nil, err // err may be *build.NoGoError - return as is
	}

	// package unsafe is known to the type checker
	if bp.ImportPath == "unsafe" {
		return types.Unsafe, nil
	}

	// no need to re-import if the package was imported completely before
	var pkg *types.Package
	if v, loaded := cachedPkgs.LoadOrStore(bp.ImportPath, &importing); loaded {
		pkg = v.(*types.Package)
	}
	if pkg != nil {
		if pkg == &importing {
			return nil, fmt.Errorf("import cycle through package %q", bp.ImportPath)
		}
		if !pkg.Complete() {
			// Package exists but is not complete - we cannot handle this
			// at the moment since the source importer replaces the package
			// wholesale rather than augmenting it (see #19337 for details).
			// Return incomplete package with error (see #16088).
			return pkg, fmt.Errorf("reimported partially imported package %q", bp.ImportPath)
		}
		return pkg, nil
	}

	defer func() {
		// clean up in case of error
		// TODO(gri) Eventually we may want to leave a (possibly empty)
		// package in the map in all cases (and use that package to
		// identify cycles). See also issue 16088.
		if v, loaded := cachedPkgs.Load(bp.ImportPath); loaded {
			if v.(*types.Package) == &importing {
				cachedPkgs.Delete(bp.ImportPath)
			}
		}
	}()

	// collect package files
	bp, err = p.ctxt.ImportDir(bp.Dir, 0)
	if err != nil {
		return nil, err // err may be *build.NoGoError - return as is
	}
	var filenames []string
	filenames = append(filenames, bp.GoFiles...)
	filenames = append(filenames, bp.CgoFiles...)

	files, err := p.parseFiles(bp.Dir, filenames)
	if err != nil {
		return nil, err
	}

	// type-check package files
	var firstHardErr error
	conf := types.Config{
		IgnoreFuncBodies: true,
		FakeImportC:      true,
		// continue type-checking after the first error
		Error: func(err error) {
			if firstHardErr == nil && !err.(types.Error).Soft {
				firstHardErr = err
			}
		},
		Importer: p,
		Sizes:    p.sizes,
	}
	pkg, err = conf.Check(bp.ImportPath, p.fset, files, nil)
	if err != nil {
		// If there was a hard error it is possibly unsafe
		// to use the package as it may not be fully populated.
		// Do not return it (see also #20837, #20855).
		if firstHardErr != nil {
			pkg = nil
			err = firstHardErr // give preference to first hard error over any soft error
		}
		return pkg, fmt.Errorf("type-checking package %q failed (%v)", bp.ImportPath, err)
	}
	if firstHardErr != nil {
		// this can only happen if we have a bug in go/types
		panic("package is not safe yet no error was returned")
	}

	fmt.Println("cached: ", bp.ImportPath)
	cachedPkgs.Store(bp.ImportPath, pkg)
	return pkg, nil
}

func (p *Importer) parseFiles(dir string, filenames []string) ([]*ast.File, error) {
	open := p.ctxt.OpenFile // possibly nil

	files := make([]*ast.File, len(filenames))
	errors := make([]error, len(filenames))

	var wg sync.WaitGroup
	wg.Add(len(filenames))
	for i, filename := range filenames {
		go func(i int, filepath string) {
			defer wg.Done()
			if open != nil {
				src, err := open(filepath)
				if err != nil {
					errors[i] = fmt.Errorf("opening package file %s failed (%v)", filepath, err)
					return
				}
				files[i], errors[i] = parser.ParseFile(p.fset, filepath, src, 0)
				src.Close() // ignore Close error - parsing may have succeeded which is all we need
			} else {
				// Special-case when ctxt doesn't provide a custom OpenFile and use the
				// parser's file reading mechanism directly. This appears to be quite a
				// bit faster than opening the file and providing an io.ReaderCloser in
				// both cases.
				// TODO(gri) investigate performance difference (issue #19281)
				files[i], errors[i] = parser.ParseFile(p.fset, filepath, nil, 0)
			}
		}(i, p.joinPath(dir, filename))
	}
	wg.Wait()

	// if there are errors, return the first one for deterministic results
	for _, err := range errors {
		if err != nil {
			return nil, err
		}
	}

	return files, nil
}

// context-controlled file system operations

func (p *Importer) absPath(path string) (string, error) {
	// TODO(gri) This should be using p.ctxt.AbsPath which doesn't
	// exist but probably should. See also issue #14282.
	return filepath.Abs(path)
}

func (p *Importer) isAbsPath(path string) bool {
	if f := p.ctxt.IsAbsPath; f != nil {
		return f(path)
	}
	return filepath.IsAbs(path)
}

func (p *Importer) joinPath(elem ...string) string {
	if f := p.ctxt.JoinPath; f != nil {
		return f(elem...)
	}
	return filepath.Join(elem...)
}
