/*
Copyright 2015 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

// A simple binary for merging PR that match a criteria
// Usage:
//   submit-queue -token=<github-access-token> -user-whitelist=<file> --jenkins-host=http://some.host [-min-pr-number=<number>] [-dry-run] [-once]
//
// Details:
/*
Usage of ./submit-queue:
  -alsologtostderr=false: log to standard error as well as files
  -dry-run=false: If true, don't actually merge anything
  -jenkins-job="kubernetes-e2e-gce,kubernetes-e2e-gke-ci,kubernetes-build": Comma separated list of jobs in Jenkins to use for stability testing
  -log_backtrace_at=:0: when logging hits line file:N, emit a stack trace
  -log_dir="": If non-empty, write log files in this directory
  -logtostderr=false: log to standard error instead of files
  -min-pr-number=0: The minimum PR to start with [default: 0]
  -once=false: If true, only merge one PR, don't run forever
  -stderrthreshold=0: logs at or above this threshold go to stderr
  -token="": The OAuth Token to use for requests.
  -user-whitelist="": Path to a whitelist file that contains users to auto-merge.  Required.
  -v=0: log level for V logs
  -vmodule=: comma-separated list of pattern=N settings for file-filtered logging
*/

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"k8s.io/contrib/submit-queue/github"
	"k8s.io/contrib/submit-queue/jenkins"

	"github.com/golang/glog"
	github_api "github.com/google/go-github/github"
)

var (
	token             = flag.String("token", "", "The OAuth Token to use for requests.")
	tokenFile         = flag.String("token-file", "", "The file containing the OAuth Token to use for requests.")
	minPRNumber       = flag.Int("min-pr-number", 0, "The minimum PR to start with [default: 0]")
	dryrun            = flag.Bool("dry-run", false, "If true, don't actually merge anything")
	oneOff            = flag.Bool("once", false, "If true, only merge one PR, don't run forever")
	jobs              = flag.String("jenkins-jobs", "kubernetes-e2e-gce,kubernetes-e2e-gke-ci,kubernetes-build", "Comma separated list of jobs in Jenkins to use for stability testing")
	jenkinsHost       = flag.String("jenkins-host", "", "The URL for the jenkins job to watch")
	userWhitelist     = flag.String("user-whitelist", "", "Path to a whitelist file that contains users to auto-merge.  Required.")
	requiredContexts  = flag.String("required-contexts", "cla/google,Shippable,continuous-integration/travis-ci/pr", "Comma separate list of status contexts required for a PR to be considered ok to merge")
	whitelistOverride = flag.String("whitelist-override-label", "ok-to-merge", "Github label, if present on a PR it will be merged even if the author isn't in the whitelist")
	pollPeriod        = flag.Duration("poll-period", 30*time.Minute, "The period for running the submit-queue.  Default 30 minutes")
	address           = flag.String("address", ":8080", "The address to listen on for HTTP Status")
	dontRequireE2E    = flag.String("dont-require-e2e-label", "e2e-not-required", "If non-empty, a PR with this label will be merged automatically without looking at e2e results")
	e2eStatusContext  = flag.String("e2e-status-context", "Jenkins GCE e2e", "The name of the github status context for the e2e PR Builder")
)

const (
	org     = "kubernetes"
	project = "kubernetes"
)

type e2eTester struct {
	sync.Mutex
	// exported so that the json marshaller will print them
	CurrentPR *github_api.PullRequest
	Message   []string
	Err       error
}

func (e *e2eTester) msg(msg string) {
	e.Lock()
	defer e.Unlock()
	if len(e.Message) > 50 {
		e.Message = e.Message[1:]
	}
	e.Message = append(e.Message, msg)
	glog.V(2).Info(msg)
}

func (e *e2eTester) error(err error) {
	e.Lock()
	defer e.Unlock()
	e.Err = err
}

// This is called on a potentially mergeable PR
func (e *e2eTester) runE2ETests(client *github_api.Client, pr *github_api.PullRequest, issue *github_api.Issue) error {
	func() {
		e.Lock()
		defer e.Unlock()
		e.CurrentPR = pr
	}()
	// Test if the build is stable in Jenkins
	jenkinsClient := &jenkins.JenkinsClient{Host: *jenkinsHost}
	builds := strings.Split(*jobs, ",")
	for _, build := range builds {
		stable, err := jenkinsClient.IsBuildStable(build)
		e.msg(fmt.Sprintf("Checking build stability for %s", build))
		if err != nil {
			e.error(err)
			return err
		}
		if !stable {
			glog.Errorf("Build %s isn't stable, skipping!", build)
			err := errors.New("Unstable build")
			e.error(err)
			return err
		}
	}
	e.msg("Build is stable.")
	// if there is a 'safe-to-merge' label, just merge it.
	if len(*dontRequireE2E) > 0 && github.HasLabel(issue.Labels, *dontRequireE2E) {
		e.msg(fmt.Sprintf("Merging %d since %s is set", *pr.Number, *dontRequireE2E))
		return e.merge(client, org, project, pr)
	}
	// Ask for a fresh build
	e.msg(fmt.Sprintf("Asking PR builder to build %d", *pr.Number))
	body := "@k8s-bot test this [testing build queue, sorry for the noise]"
	if _, _, err := client.Issues.CreateComment(org, project, *pr.Number, &github_api.IssueComment{Body: &body}); err != nil {
		e.error(err)
		return err
	}

	// Wait for the build to start
	err := github.WaitForPending(client, org, project, *pr.Number)

	// Wait for the status to go back to 'success'
	ok, err := github.ValidateStatus(client, org, project, *pr.Number, []string{}, true)
	if err != nil {
		e.error(err)
		return err
	}
	if !ok {
		e.msg(fmt.Sprintf("Status after build is not 'success', skipping PR %d", *pr.Number))
		return nil
	}
	return e.merge(client, org, project, pr)
}

func (e *e2eTester) merge(client *github_api.Client, org, project string, pr *github_api.PullRequest) error {
	if !*dryrun {
		e.msg(fmt.Sprintf("Merging PR: %d", *pr.Number))
		mergeBody := "Automatic merge from SubmitQueue"
		if _, _, err := client.Issues.CreateComment(org, project, *pr.Number, &github_api.IssueComment{Body: &mergeBody}); err != nil {
			glog.Warningf("Failed to create merge comment: %v", err)
			e.error(err)
			return err
		}
		if _, _, err := client.PullRequests.Merge(org, project, *pr.Number, "Auto commit by PR queue bot"); err != nil {
			e.error(err)
			return err
		}
		return nil
	}
	e.msg("Skipping actual merge because --dry-run is set")
	return nil
}

func (e *e2eTester) ServeHTTP(res http.ResponseWriter, req *http.Request) {
	e.Lock()
	defer e.Unlock()
	res.Header().Set("Content-type", "application/json")
	if data, err := json.Marshal(e); err != nil {
		res.WriteHeader(http.StatusInternalServerError)
		res.Write([]byte(err.Error()))
	} else {
		res.WriteHeader(http.StatusOK)
		res.Write(data)
	}
}

func loadWhitelist(file string) ([]string, error) {
	if len(file) == 0 {
		return []string{}, nil
	}
	fp, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer fp.Close()
	scanner := bufio.NewScanner(fp)
	result := []string{}
	for scanner.Scan() {
		result = append(result, scanner.Text())
	}
	return result, scanner.Err()
}

func main() {
	flag.Parse()
	if len(*jenkinsHost) == 0 {
		glog.Fatalf("--jenkins-host is required.")
	}
	tokenData := *token
	if len(tokenData) == 0 && len(*tokenFile) != 0 {
		data, err := ioutil.ReadFile(*tokenFile)
		if err != nil {
			glog.Fatalf("error reading token file: %v", err)
		}
		tokenData = string(data)
	}

	client := github.MakeClient(tokenData)

	users, err := loadWhitelist(*userWhitelist)
	if err != nil {
		glog.Fatalf("error loading user whitelist: %v", err)
	}
	requiredContexts := strings.Split(*requiredContexts, ",")
	config := &github.FilterConfig{
		MinPRNumber:             *minPRNumber,
		AdditionalUserWhitelist: users,
		RequiredStatusContexts:  requiredContexts,
		WhitelistOverride:       *whitelistOverride,
		DryRun:                  *dryrun,
		DontRequireE2ELabel:     *dontRequireE2E,
		E2EStatusContext:        *e2eStatusContext,
	}
	e2e := &e2eTester{}
	if len(*address) > 0 {
		go http.ListenAndServe(*address, e2e)
	}
	for !*oneOff {
		if err := github.ForEachCandidatePRDo(client, org, project, e2e.runE2ETests, *oneOff, config); err != nil {
			glog.Errorf("Error getting candidate PRs: %v", err)
		}
		time.Sleep(*pollPeriod)
	}
}
