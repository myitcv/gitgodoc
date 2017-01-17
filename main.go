// Copyright (c) 2016 Paul Jolly <paul@myitcv.org.uk>, all rights reserved.
// Use of this document is governed by a license found in the LICENSE document.

// gitgodoc allows you to view `godoc` documentation for different branches of a Git repository
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// TODO make cross-platform

// TODO implement pruning of remote branches that no longer exist (we should be able to detect after each
// push)

type branchPorts map[string]uint
type branchLocks map[string]*sync.Mutex

const (
	refCopy = "@refcopy"

	debug = false
)

var (
	validBranch = regexp.MustCompile("^[a-z0-9_-]+$")
	href        = regexp.MustCompile(`(href|src)="/("|[^/][^"]*")`)

	fServeFrom = flag.String("serveFrom", "", "directory to use as a working directory")
	fRepo      = flag.String("repo", "", "git url from which to clone")
	fPkg       = flag.String("pkg", "", "the package the repo represents")
	fGoPath    = flag.String("gopath", "", "a relative GOPATH that will be used when running the godoc server (prepended with the repo dir)")
	fPortStart = flag.Uint("port", 8080, "the port on which to serve; controlled godoc instances will be started on subsequent ports")
)

// TODO this should be split into separate types and the correct
// type used based on the header sent from Gitlab
type gitLabWebhook struct {
	ObjectKind       string `json:"object_kind"`
	Ref              string `json:"ref"`
	ObjectAttributes struct {
		MergeStatus  string `json:"merge_status"`
		SourceBranch string `json:"source_branch"`
		TargetBranch string `json:"target_branch"`
	} `json:"object_attributes"`
}

type server struct {
	repo, serveFrom, pkg, refCopyDir string
	gopath                           []string

	nextPort uint

	ports     atomic.Value
	portsLock sync.Mutex

	repos     atomic.Value
	reposLock sync.Mutex
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("gitgodoc ")

	flag.Parse()

	s := newServer()

	s.setupRefCopy()
	s.serve()
}

func (s *server) setupRefCopy() {
	infof("setting up reference copy of repo in %v", s.refCopyDir)

	s.reposLock.Lock()
	defer s.reposLock.Unlock()

	if _, err := os.Stat(filepath.Join(s.refCopyDir, ".git")); err != nil {
		// the copy does not exist
		// no need to do a fetch; instead we need to clone

		git(s.serveFrom, "clone", s.repo, s.refCopyDir)
		git(s.refCopyDir, "checkout", "-f", "origin/master")
	} else {
		git(s.refCopyDir, "fetch")
		git(s.refCopyDir, "checkout", "-f", "origin/master")
	}
}

// TODO break this apart
func (s *server) serve() {
	tr := &http.Transport{
		DisableCompression:  true,
		DisableKeepAlives:   false,
		MaxIdleConnsPerHost: 20,
	}
	client := &http.Client{Transport: tr}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {

		if !path.IsAbs(r.URL.Path) {
			fatalf("expected absolute URL path, got %v", r.URL.Path)
		}

		if r.URL.Path == "/" {
			if r.Method == http.MethodPost {
				if vs, ok := r.URL.Query()["refresh"]; ok {

					// TODO support more than just gitlab
					if len(vs) == 1 && vs[0] == "gitlab" {
						// we need to parse the branch from the request

						body, err := ioutil.ReadAll(r.Body)
						if err != nil {
							fatalf("failed to read body of POST request")
						}
						var pHook gitLabWebhook
						err = json.Unmarshal(body, &pHook)

						if err != nil {
							fatalf("could not decode Gitlab web")
						}

						switch pHook.ObjectKind {
						case "merge_request":
							s.fetch(pHook.ObjectAttributes.SourceBranch)

							infof("got a request to refresh branch %v in response to a merge request hook with target %v and merge status %v", pHook.ObjectAttributes.SourceBranch, pHook.ObjectAttributes.TargetBranch, pHook.ObjectAttributes.MergeStatus)

						case "push":
							ref := strings.Split(pHook.Ref, "/")
							if len(ref) != 3 {
								fatalf("did not understand format of branch: %v", pHook.Ref)
							}

							branch := ref[2]

							infof("got a request to refresh branch %v in response to a push hook", branch)

							s.fetch(branch)
						default:
							w.WriteHeader(http.StatusInternalServerError)

							msg := fmt.Sprintf("Did not understand Gitlab refresh request; unknown object_kind: %v", pHook.ObjectKind)
							infof(msg)
							fmt.Fprintln(w, msg)

							return
						}

						w.WriteHeader(http.StatusOK)
						w.Write([]byte("OK\n"))

						return
					} else if len(vs) == 1 && vs[0] == "gitlab_merge_request" {

					} else {
						w.WriteHeader(http.StatusInternalServerError)

						msg := fmt.Sprintf("Did not understand refresh request: %v", r.URL)
						infof(msg)
						fmt.Fprintln(w, msg)

						return
					}
				}
			}

			// we should serve a simple page of links to existing branches
			w.WriteHeader(http.StatusOK)
			var bs []string
			m := s.repos.Load().(branchLocks)
			for k := range m {
				bs = append(bs, k)
			}

			sort.Strings(bs)

			tmpl := struct {
				Branches []string
				Pkg      string
			}{
				Branches: bs,
				Pkg:      s.pkg,
			}
			tpl := `
{{ $pkg := .Pkg }}
<!DOCTYPE html>
<html>
	<head>
		<meta charset="UTF-8">
		<title>gitgodoc server for {{.Pkg}}</title>
	</head>
	<body>
		<h1><code>gitgodoc</code> server for <code>{{.Pkg}}</code></h1>
		{{with .Branches}}
			{{ range . }}{{ if eq . "master" }}<p><a href="/{{.}}">{{ . }}</a> (<a href="/{{.}}/pkg/{{$pkg}}/">{{$pkg}}</a>)</p>{{ end }}{{ end }}
			<ul>
			{{ range . }}{{ if ne . "master" }}<li><a href="/{{.}}">{{ . }}</a> (<a href="/{{.}}/pkg/{{$pkg}}/">{{$pkg}}</a>)</li>{{ end }}{{ end }}
			</ul>
		{{else}}
			<div><strong>No branches known to godoc server</strong></div>
		{{end}}
	</body>
</html>`

			t, err := template.New("webpage").Parse(tpl)
			if err != nil {
				fatalf("could not parse branch browser template: %v", err)
			}

			t.Execute(w, tmpl)

			return
		}

		parts := strings.Split(r.URL.Path, "/")
		branch := parts[1]
		actUrl := path.Join(parts[2:]...)

		if !validBranch.MatchString(branch) {
			branch = "master"

			// now we need to redirect to the same URL but with a branch
			newUrl := *r.URL
			newUrl.Path = "/" + path.Join(branch, actUrl)

			http.Redirect(w, r, newUrl.String(), http.StatusFound)

			return
		}

		debugf("got request to %v; branch %v ", r.URL, branch)

		if be := s.branchExists(branch); !be {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, "Branch %v not found\n", branch)

			return
		}

		port := s.getPort(branch)

		scheme := r.URL.Scheme
		if scheme == "" {
			scheme = "http"
		}

		var host string
		if r.URL.Host != "" {
			hostParts := strings.Split(r.URL.Host, ":")
			host = hostParts[0]
		}
		url := fmt.Sprintf("%v://%v:%v/%v", scheme, host, port, actUrl)

		debugf("making onward request to %v", url)

		req, err := http.NewRequest(r.Method, url, r.Body)
		if err != nil {
			fatalf("could not create proxy request to %v: %v", url, err)
		}

		resp, err := client.Do(req)
		if err != nil {
			fatalf("could not do proxy req to %v: %v", url, err)
		}

		isHtml := false

		wh := w.Header()
		for k, v := range resp.Header {
			if k == "Content-Type" {
				for _, vv := range v {
					ct := strings.Split(vv, ";")
					for _, p := range ct {
						p = strings.TrimSpace(p)
						if p == "text/html" {
							isHtml = true
						}
					}
				}

			}
			wh[k] = v
		}

		w.WriteHeader(resp.StatusCode)

		if !isHtml {
			_, err = io.Copy(w, resp.Body)
			if err != nil {
				fatalf("could not relay body in response to proxy req to %v: %v", url, err)
			}
		} else {
			// TODO shouldn't have to rewrite if we can get the godoc server to
			// use a unique Path root
			sc := bufio.NewScanner(resp.Body)

			repl := "$1=\"/" + branch + "/$2"

			for sc.Scan() {
				nl := href.ReplaceAllString(sc.Text(), repl)
				fmt.Fprintln(w, nl)
			}

			if err := sc.Err(); err != nil {
				fatalf("error scanning response body: %v", err)
			}
		}

		fmt.Fprintf(w, "OK")
	})

	url := fmt.Sprintf(":%v", s.nextPort)

	infof("starting server %v", url)

	err := http.ListenAndServe(url, nil)
	if err != nil {
		fatalf("failed to start main server on %v: %v", url, err)
	}
}

func newServer() *server {
	if *fServeFrom == "" || *fRepo == "" || *fPkg == "" {
		flag.Usage()
		os.Exit(1)
	}

	// TODO validate *fPkg

	s := &server{
		nextPort: *fPortStart,
		pkg:      *fPkg,
		repo:     *fRepo,
	}

	serveFrom, err := filepath.Abs(*fServeFrom)
	if err != nil {
		fatalf("could not make absolute filepath from %v: %v", *fServeFrom, err)
	}

	if sd, err := os.Stat(serveFrom); err == nil {
		if !sd.IsDir() {
			fatalf("%v exists but is not a directory", serveFrom)
		}
	} else {
		err := os.Mkdir(serveFrom, 0700)
		if err != nil {
			fatalf("could not mkdir %v: %v", serveFrom, err)
		}
	}
	s.serveFrom = serveFrom

	if *fGoPath != "" {
		parts := filepath.SplitList(*fGoPath)

		for _, p := range parts {
			if filepath.IsAbs(p) {
				fatalf("gopath flag contains non-relative part %v", p)
			}

			s.gopath = append(s.gopath, p)
		}
	} else {
		s.gopath = []string{""}
	}

	s.refCopyDir = filepath.Join(s.serveFrom, refCopy)

	s.ports.Store(make(branchPorts))
	s.repos.Store(make(branchLocks))

	return s
}

func (s *server) getPort(branch string) uint {
	m := s.ports.Load().(branchPorts)
	port, gdRunning := m[branch]

	if gdRunning {
		return port
	}

	s.portsLock.Lock()
	defer s.portsLock.Unlock()

	// check again
	m = s.ports.Load().(branchPorts)
	port, gdRunning = m[branch]

	if gdRunning {
		return port
	}

	s.nextPort++
	port = s.nextPort

	mp := make(branchPorts)
	for k, v := range m {
		mp[k] = v
	}
	mp[branch] = port
	defer s.ports.Store(mp)

	s.runGoDoc(branch, port)

	// TODO this is pretty gross
	time.Sleep(500 * time.Millisecond)

	// now "signal" we are done setting up this server

	return port
}

func (s *server) branchExists(branch string) bool {
	m := s.repos.Load().(branchLocks)
	_, ok := m[branch]

	return ok
}

func (s *server) setup(branch string, cloneIfMissing bool) bool {
	m := s.repos.Load().(branchLocks)
	_, ok := m[branch]

	if ok {
		return true
	}

	s.reposLock.Lock()
	defer s.reposLock.Unlock()

	//check again now we are criticl
	m = s.repos.Load().(branchLocks)
	_, ok = m[branch]

	if ok {
		return true
	}

	ml := make(branchLocks)
	for k, v := range m {
		ml[k] = v
	}
	ml[branch] = new(sync.Mutex)
	defer s.repos.Store(ml)

	ct := s.gitDir(branch)

	if _, err := os.Stat(filepath.Join(ct, ".git")); err == nil {
		return true
	}

	if !cloneIfMissing {
		infof("branch %v does not exist; told not to copy", branch)
		return false
	}

	// keep the refCopy fresh
	git(s.refCopyDir, "fetch")

	// ensure the path to the target exists
	p := filepath.Dir(ct)
	err := os.MkdirAll(p, 0700)
	if err != nil {
		fatalf("could not create branch directory structure %v: %v", p, err)
	}

	// TODO - this is not cross platform
	infof("copying refcopy cp -pr %v %v", s.refCopyDir, ct)
	cmd := exec.Command("cp", "-rp", s.refCopyDir, ct)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fatalf("could not copy refcopy %v to branch: %v\n%v", ct, err, string(out))
	}

	// now ensure we're on that branch in the copy
	git(ct, "checkout", "-f", "origin/"+branch)

	// now "signal" that we are done setting up this branch

	return false
}

func (s *server) fetch(branch string) {
	exists := s.setup(branch, true)
	if exists {
		m := s.repos.Load().(branchLocks)
		pl, ok := m[branch]

		if !ok {
			fatalf("no lock exists for branch?")
		}

		pl.Lock()
		defer pl.Unlock()

		gd := s.gitDir(branch)

		git(gd, "fetch")
		git(gd, "checkout", "-f", "origin/"+branch)
	}
}

func (s *server) branchDir(branch string) string {
	return filepath.Join(s.serveFrom, branch)
}

func (s *server) gitDir(branch string) string {
	return filepath.Join(s.branchDir(branch), "src", s.pkg)
}

func (s *server) buildGoPath(branch string) string {
	var res string
	sep := ""
	for _, p := range s.gopath {
		res = res + sep + filepath.Join(s.serveFrom, branch, p)
		sep = string(filepath.ListSeparator)
	}

	return res
}

func git(cwd string, args ...string) {
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd

	infof("running git %v (CWD %v)", strings.Join(args, " "), cwd)

	out, err := cmd.CombinedOutput()
	if err != nil {
		fatalf("failed to run command: git %v: %v\n%v", strings.Join(args, " "), err, string(out))
	}
}

func (s *server) runGoDoc(branch string, port uint) {
	go func() {
		gp := s.buildGoPath(branch)

		attrs := &syscall.SysProcAttr{
			Pdeathsig: syscall.SIGTERM,
		}

		env := []string{
			"GOROOT=" + runtime.GOROOT(),
			"GOPATH=" + gp,
		}
		portStr := fmt.Sprintf(":%v", port)
		cmd := exec.Command("godoc", "-http", portStr)
		cmd.Env = env
		cmd.SysProcAttr = attrs

		infof("starting godoc server on port %v with env %v", portStr, env)

		out, err := cmd.CombinedOutput()
		if err != nil {
			fatalf("godoc -http %v failed: %v\n%v", portStr, err, string(out))
		}
	}()
}

func infof(format string, args ...interface{}) {
	log.Printf(format, args...)
}

func debugf(format string, args ...interface{}) {
	if debug {
		log.Printf(format, args...)
	}
}

func fatalf(format string, args ...interface{}) {
	log.Fatalf(format, args...)
}
