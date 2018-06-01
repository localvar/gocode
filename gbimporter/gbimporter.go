package gbimporter

import (
	"fmt"
	"go/build"
	stdimporter "go/importer"
	"go/types"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// We need to mangle go/build.Default to make gcimporter work as
// intended, so use a lock to protect against concurrent accesses.
var buildDefaultLock sync.Mutex

// importer implements types.ImporterFrom and provides transparent
// support for gb-based projects.
type importer struct {
	underlying types.ImporterFrom
	ctx        *PackedContext
	gbroot     string
	gbpaths    []string
}

func New(ctx *PackedContext, filename string) types.ImporterFrom {
	imp := &importer{ctx: ctx}

	slashed := filepath.ToSlash(filename)
	i := strings.LastIndex(slashed, "/vendor/src/")
	if i < 0 {
		i = strings.LastIndex(slashed, "/src/")
	}
	if i > 0 {
		cachedPkgs.setCurrentPackage(slashed[i+5:])
		paths := filepath.SplitList(imp.ctx.GOPATH)

		gbroot := filepath.FromSlash(slashed[:i])
		gbvendor := filepath.Join(gbroot, "vendor")
		if gbroot == imp.ctx.GOROOT {
			goto Found
		}
		for _, path := range paths {
			if path == gbroot || path == gbvendor {
				goto Found
			}
		}

		imp.gbroot = gbroot
		imp.gbpaths = append(paths, gbroot, gbvendor)
	Found:
	}

	imp.underlying = stdimporter.For("source", nil).(types.ImporterFrom)
	return imp
}

func (i *importer) Import(path string) (*types.Package, error) {
	return i.ImportFrom(path, "", 0)
}

type dirCache struct {
	lastAccess time.Time
	pkgs       map[string]*types.Package
}

type pkgCache struct {
	curPkg string
	dirs   map[string]*dirCache
}

func (pc *pkgCache) setCurrentPackage(file string) {
	if i := strings.LastIndexByte(file, '/'); i <= 0 {
		return
	} else if pc.curPkg == file[:i] {
		return
	} else {
		pc.curPkg = file[:i]
	}

	for _, d := range pc.dirs {
		for n := range d.pkgs {
			if n == pc.curPkg {
				delete(d.pkgs, n)
				break
			}
		}
	}
}

func (pc *pkgCache) search(srcDir, path string) *types.Package {
	if d, ok := pc.dirs[srcDir]; ok {
		if p, ok := d.pkgs[path]; ok {
			d.lastAccess = time.Now()
			return p
		}
	}
	return nil
}

func (pc *pkgCache) add(srcDir, path string, pkg *types.Package) {
	if d, ok := pc.dirs[srcDir]; ok {
		d.pkgs[path] = pkg
		d.lastAccess = time.Now()
		return
	}

	if len(pc.dirs) >= 10 {
		oldest, t := "", time.Now().Add(10000*time.Hour)
		for k, v := range pc.dirs {
			if v.lastAccess.Before(t) {
				t = v.lastAccess
				oldest = k
			}
		}
		delete(pc.dirs, oldest)
	}

	pc.dirs[srcDir] = &dirCache{
		lastAccess: time.Now(),
		pkgs:       map[string]*types.Package{path: pkg},
	}
}

var cachedPkgs = pkgCache{dirs: map[string]*dirCache{}}

func (i *importer) ImportFrom(path, srcDir string, mode types.ImportMode) (*types.Package, error) {
	buildDefaultLock.Lock()
	defer buildDefaultLock.Unlock()

	if p := cachedPkgs.search(srcDir, path); p != nil {
		return p, nil
	}

	origDef := build.Default
	defer func() { build.Default = origDef }()

	def := &build.Default
	def.GOARCH = i.ctx.GOARCH
	def.GOOS = i.ctx.GOOS
	def.GOROOT = i.ctx.GOROOT
	def.GOPATH = i.ctx.GOPATH
	def.CgoEnabled = i.ctx.CgoEnabled
	def.UseAllFiles = i.ctx.UseAllFiles
	def.Compiler = i.ctx.Compiler
	def.BuildTags = i.ctx.BuildTags
	def.ReleaseTags = i.ctx.ReleaseTags
	def.InstallSuffix = i.ctx.InstallSuffix

	def.SplitPathList = i.splitPathList
	def.JoinPath = i.joinPath

	p, e := i.underlying.ImportFrom(path, srcDir, mode)
	if e != nil {
		return p, e
	}

	cachedPkgs.add(srcDir, path, p)
	return p, nil
}

func (i *importer) splitPathList(list string) []string {
	if i.gbroot != "" {
		return i.gbpaths
	}
	return filepath.SplitList(list)
}

func (i *importer) joinPath(elem ...string) string {
	res := filepath.Join(elem...)

	if i.gbroot != "" {
		// Want to rewrite "$GBROOT/(vendor/)?pkg/$GOOS_$GOARCH(_)?"
		// into "$GBROOT/pkg/$GOOS-$GOARCH(-)?".
		// Note: gb doesn't use vendor/pkg.
		if gbrel, err := filepath.Rel(i.gbroot, res); err == nil {
			gbrel = filepath.ToSlash(gbrel)
			gbrel, _ = match(gbrel, "vendor/")
			if gbrel, ok := match(gbrel, fmt.Sprintf("pkg/%s_%s", i.ctx.GOOS, i.ctx.GOARCH)); ok {
				gbrel, hasSuffix := match(gbrel, "_")

				// Reassemble into result.
				if hasSuffix {
					gbrel = "-" + gbrel
				}
				gbrel = fmt.Sprintf("pkg/%s-%s/", i.ctx.GOOS, i.ctx.GOARCH) + gbrel
				gbrel = filepath.FromSlash(gbrel)
				res = filepath.Join(i.gbroot, gbrel)
			}
		}
	}

	return res
}

func match(s, prefix string) (string, bool) {
	rest := strings.TrimPrefix(s, prefix)
	return rest, len(rest) < len(s)
}
