/*
Copyright 2021 Gravitational, Inc.

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

package review

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/gravitational/teleport/.github/workflows/robot/internal/github"

	"github.com/gravitational/trace"
)

// Reviewer is a code reviewer.
type Reviewer struct {
	// Team the reviewer belongs to.
	Team string `json:"team"`
	// Owner is true if the reviewer is a code or docs owner (required for all reviews).
	Owner bool `json:"owner"`
	// GithubUsername is the reviewer's Github username
	GithubUsername string `json:"username"`
}

// Config holds code reviewer configuration.
type Config struct {
	// Rand is a random number generator. It is not safe for cryptographic
	// operations.
	Rand *rand.Rand

	// CodeReviewers and CodeReviewersOmit is a map of code reviews and code
	// reviewers to omit.
	CodeReviewers     map[string]Reviewer `json:"codeReviewers"`
	CodeReviewersOmit map[string]bool     `json:"codeReviewersOmit"`

	// DocsReviewers and DocsReviewersOmit is a map of docs reviews and docs
	// reviewers to omit.
	DocsReviewers     map[string]Reviewer `json:"docsReviewers"`
	DocsReviewersOmit map[string]bool     `json:"docsReviewersOmit"`

	// Admins are assigned reviews when no others match.
	Admins []string `json:"admins"`

	// ripplingToken is the Rippling API token.
	ripplingToken string
}

// CheckAndSetDefaults checks and sets defaults.
func (c *Config) CheckAndSetDefaults() error {
	if c.Rand == nil {
		c.Rand = rand.New(rand.NewSource(time.Now().UnixNano()))
	}

	if c.CodeReviewers == nil {
		return trace.BadParameter("missing parameter CodeReviewers")
	}
	if c.CodeReviewersOmit == nil {
		return trace.BadParameter("missing parameter CodeReviewersOmit")
	}

	if c.DocsReviewers == nil {
		return trace.BadParameter("missing parameter DocsReviewers")
	}
	if c.DocsReviewersOmit == nil {
		return trace.BadParameter("missing parameter DocsReviewersOmit")
	}

	if c.Admins == nil {
		return trace.BadParameter("missing parameter Admins")
	}

	if c.ripplingToken == "" {
		return trace.BadParameter("missing parameter Token")
	}

	return nil
}

// Assignments can be used to assign and check code reviewers.
type Assignments struct {
	c             *Config
	leaveRequests map[string]bool
}

// FromString parses JSON formatted configuration and returns assignments.
func FromString(reviewers string, token string) (*Assignments, error) {
	var c Config
	if err := json.Unmarshal([]byte(reviewers), &c); err != nil {
		return nil, trace.Wrap(err)
	}
	c.ripplingToken = token
	r, err := New(&c)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return r, nil
}

// New returns new code review assignments.
func New(c *Config) (*Assignments, error) {
	if err := c.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}
	onLeave, err := getLeaveMap(c.ripplingToken)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return &Assignments{
		c:             c,
		leaveRequests: onLeave,
	}, nil
}

// IsInternal returns if the author of a PR is internal.
func (r *Assignments) IsInternal(author string) bool {
	for _, reviewer := range r.c.CodeReviewers {
		if reviewer.GithubUsername == author {
			return true
		}
	}
	for _, reviewer := range r.c.DocsReviewers {
		if reviewer.GithubUsername == author {
			return true
		}
	}
	return false
}

// Get will return a list of code reviewers a given author.
func (r *Assignments) Get(author string, docs bool, code bool) []string {
	var reviewers []string

	switch {
	case docs && code:
		log.Printf("Assign: Found docs and code changes.")
		reviewers = append(reviewers, r.getDocsReviewers(author)...)
		reviewers = append(reviewers, r.getCodeReviewers(author)...)
	case !docs && code:
		log.Printf("Assign: Found code changes.")
		reviewers = append(reviewers, r.getCodeReviewers(author)...)
	case docs && !code:
		log.Printf("Assign: Found docs changes.")
		reviewers = append(reviewers, r.getDocsReviewers(author)...)
	// Strange state, an empty commit? Return admin reviewers.
	case !docs && !code:
		log.Printf("Assign: Found no docs or code changes.")
		reviewers = append(reviewers, r.getAdminReviewers(author)...)
	}

	return reviewers
}

func (r *Assignments) getDocsReviewers(author string) []string {
	setA, setB := getReviewerSets(author, "Core", r.c.DocsReviewers, r.c.DocsReviewersOmit, nil)
	reviewers := append(setA, setB...)

	// If no docs reviewers were assigned, assign admin reviews.
	if len(reviewers) == 0 {
		return r.getAdminReviewers(author)
	}
	return reviewers
}

func (r *Assignments) getCodeReviewers(author string) []string {
	setA, setB := r.getCodeReviewerSets(author)

	return []string{
		setA[r.c.Rand.Intn(len(setA))],
		setB[r.c.Rand.Intn(len(setB))],
	}
}

func (r *Assignments) getAdminReviewers(author string) []string {
	var reviewers []string
	for _, v := range r.c.Admins {
		if v == author {
			continue
		}
		reviewers = append(reviewers, v)
	}
	return reviewers
}

func (r *Assignments) getCodeReviewerSets(author string) ([]string, []string) {
	// Internal non-Core contributors get assigned from the admin reviewer set.
	// Admins will review, triage, and re-assign.
	var githubAuthor *Reviewer
	for _, reviewer := range r.c.CodeReviewers {
		if reviewer.GithubUsername == author {
			githubAuthor = &reviewer
			break
		}
	}

	if githubAuthor == nil || githubAuthor.Team == "Internal" {
		reviewers := r.getAdminReviewers(author)
		n := len(reviewers) / 2
		return reviewers[:n], reviewers[n:]
	}

	return getReviewerSets(author, githubAuthor.Team, r.c.CodeReviewers, r.c.CodeReviewersOmit, nil)

}

// CheckExternal requires two admins have approved.
func (r *Assignments) CheckExternal(author string, reviews map[string]*github.Review) error {
	log.Printf("Check: Found external author %v.", author)

	reviewers := r.getAdminReviewers(author)

	if checkN(reviewers, reviews) > 1 {
		return nil
	}
	return trace.BadParameter("at least two approvals required from %v", reviewers)
}

// CheckInternal will verify if required reviewers have approved. Checks if
// docs and if each set of code reviews have approved. Admin approvals bypass
// all checks.
func (r *Assignments) CheckInternal(author string, reviews map[string]*github.Review, docs bool, code bool) error {
	log.Printf("Check: Found internal author %v.", author)

	// Skip checks if admins have approved.
	if check(r.getAdminReviewers(author), reviews) {
		return nil
	}

	switch {
	case docs && code:
		log.Printf("Check: Found docs and code changes.")
		if err := r.checkDocsReviews(author, reviews); err != nil {
			return trace.Wrap(err)
		}
		if err := r.checkCodeReviews(author, reviews); err != nil {
			return trace.Wrap(err)
		}
	case !docs && code:
		log.Printf("Check: Found code changes.")
		if err := r.checkCodeReviews(author, reviews); err != nil {
			return trace.Wrap(err)
		}
	case docs && !code:
		log.Printf("Check: Found docs changes.")
		if err := r.checkDocsReviews(author, reviews); err != nil {
			return trace.Wrap(err)
		}
	// Strange state, an empty commit? Check admins.
	case !docs && !code:
		log.Printf("Check: Found no docs or code changes.")
		if checkN(r.getAdminReviewers(author), reviews) < 2 {
			return trace.BadParameter("requires two admin approvals")
		}
	}

	return nil
}

func (r *Assignments) checkDocsReviews(author string, reviews map[string]*github.Review) error {
	reviewers := r.getDocsReviewers(author)

	if check(reviewers, reviews) {
		return nil
	}

	return trace.BadParameter("requires at least one approval from %v", reviewers)
}

func (r *Assignments) checkCodeReviews(author string, reviews map[string]*github.Review) error {
	// External code reviews should never hit this path, if they do, fail and
	// return an error.
	var rev *Reviewer
	for _, reviewer := range r.c.CodeReviewers {
		if reviewer.GithubUsername == author {
			rev = &reviewer
			break
		}
	}
	if rev == nil {
		return trace.BadParameter("rejecting checking external review")
	}

	// Internal Teleport reviews get checked by same Core rules. Other teams do
	// own internal reviews.
	team := rev.Team
	if team == "Internal" {
		team = "Core"
	}

	setA, setB := getReviewerSets(author, team, r.c.CodeReviewers, r.c.CodeReviewersOmit, nil)

	// PRs can be approved if you either have multiple code owners that approve
	// or code owner and code reviewer.
	if checkN(setA, reviews) >= 2 {
		return nil
	}
	if check(setA, reviews) && check(setB, reviews) {
		return nil
	}

	return trace.BadParameter("at least one approval required from each set %v %v", setA, setB)
}

func getReviewerSets(author string, team string, reviewers map[string]Reviewer, reviewersOmit map[string]bool, leaveRequests map[string]bool) ([]string, []string) {
	var setA []string
	var setB []string

	for _, v := range reviewers {
		// Only assign within a team.
		if v.Team != team {
			continue
		}
		// Skip over reviewers that are marked as omit.
		if _, ok := reviewersOmit[v.GithubUsername]; ok {
			continue
		}
		// Skip author, can't assign/review own PR.
		if v.GithubUsername == author {
			continue
		}

		if v.Owner {
			setA = append(setA, v.GithubUsername)
		} else {
			setB = append(setB, v.GithubUsername)
		}
	}

	return setA, setB
}

func check(reviewers []string, reviews map[string]*github.Review) bool {
	return checkN(reviewers, reviews) > 0
}

func checkN(reviewers []string, reviews map[string]*github.Review) int {
	var n int
	for _, reviewer := range reviewers {
		if review, ok := reviews[reviewer]; ok {
			if review.State == approved && review.Author == reviewer {
				n++
			}
		}
	}
	return n
}

const (
	// approved is a code review where the reviewer has approved changes.
	approved = "APPROVED"
	// changesRequested is a code review where the reviewer has requested changes.
	changesRequested = "CHANGES_REQUESTED"
)

func getLeaveMap(token string) (map[string]bool, error) {
	omit := map[string]bool{}
	now := time.Now()
	leaveRequests, err := getLeaveRequests(now, token)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	for _, req := range leaveRequests {
		if shouldOmit(now, req) {
			omit[req.FullName] = true
		}
	}
	return omit, nil
}

const (
	// layout is the Time format layout
	layout = "2006-01-02"
	// approvedLeaveRequestStatus is the status of an
	// approved leave request.
	approvedLeaveRequestStatus = "APPROVED"
)

func getLeaveRequests(now time.Time, token string) ([]EmployeeLeaveRequest, error) {
	ripplingUrl := url.URL{
		Scheme: "https",
		Host:   "api.rippling.com",
		Path:   path.Join("platform", "api", "leave_requests"),
	}

	req, err := http.NewRequest(http.MethodGet, ripplingUrl.String(), nil)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Add authorization header.
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

	// Start query 3 days in the past to get leave requests that may
	// have already ended, but still need to omit the employee from
	// reviews.
	// 3 days is needed to account for non-business days plus the 1
	// day post-leave omit period.
	startQuery := now.AddDate(0, 0, -3)
	formattedStart := fmt.Sprintf("%d-%02d-%02d",
		startQuery.Year(), startQuery.Month(), startQuery.Day())

	// End query 4 days in the future to get future leave requests of
	// the reviewers that need to be omitted.
	// 4 days is needed to account for non-business days plus the 2
	// days pre-leave omit period.
	endQuery := now.AddDate(0, 0, 4)
	formattedEnd := fmt.Sprintf("%d-%02d-%02d",
		endQuery.Year(), endQuery.Month(), endQuery.Day())

	// Set query values.
	q := req.URL.Query()
	q.Add("from", formattedStart)
	q.Add("to", formattedEnd)
	q.Add("status", approvedLeaveRequestStatus)
	req.URL.RawQuery = q.Encode()

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	var leaveRequests []EmployeeLeaveRequest
	err = json.Unmarshal([]byte(body), &leaveRequests)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return leaveRequests, nil
}

func shouldOmit(date time.Time, req EmployeeLeaveRequest) bool {
	// Leave is defined as being out for more than
	// two business days.
	if businessDaysCount(req) <= 2 {
		return false
	}

	// Subtract two days to the beginning of the leave range
	// to account for the pre-leave omit period.
	additionalLeaveStart := -2

	// Add a day to the end of the leave request range to account
	// for the post-leave omit period.
	additionalLeaveEnd := 1

	// If the request starts on a Monday or Tuesday, subtract two
	// more days to account for non-business days.
	if req.StartDate.Weekday() == time.Monday || req.StartDate.Weekday() == time.Tuesday {
		additionalLeaveStart -= 2
	}

	// If the leave request end date is a Friday, add two more days
	// to account for non-business days.
	if req.EndDate.Weekday() == time.Friday {
		additionalLeaveEnd += 2
	}

	// Subtract and add 1 day to the range so the last return statement
	// returns true if today lands on the start or end date of the
	// leave request omit period.
	start := req.StartDate.Time.AddDate(0, 0, additionalLeaveStart-1)
	end := req.EndDate.AddDate(0, 0, additionalLeaveEnd+1)

	return date.After(start) && date.Before(end)
}

// businessDaysCount gets the number of business days
// during the leave request.
func businessDaysCount(req EmployeeLeaveRequest) int {
	start, end, totalDays, weekendDays := req.StartDate, req.EndDate, 0, 0
	for !start.After(end.Time) {
		totalDays++
		if start.Weekday() == time.Saturday || start.Weekday() == time.Sunday {
			weekendDays++
		}
		start.Time = start.AddDate(0, 0, 1)
	}
	return totalDays - weekendDays
}

// UnmarshalJSON unmarshals a string in the format of
func (t *Time) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	timeToParse := strings.Trim(string(b[:]), "\"")
	date, err := time.Parse(layout, timeToParse)
	if err != nil {
		return trace.Wrap(err)
	}
	*t = Time{date}
	return nil
}

type Time struct {
	time.Time
}

type EmployeeLeaveRequest struct {
	FullName  string `json:"requestedByName"`
	StartDate Time   `json:"startDate"`
	EndDate   Time   `json:"endDate"`
}
