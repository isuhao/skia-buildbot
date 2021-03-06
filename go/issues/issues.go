package issues

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"

	"github.com/skia-dev/glog"
	"go.skia.org/infra/go/util"
)

const (
	MONORAIL_BASE_URL = "https://monorail-prod.appspot.com/_ah/api/monorail/v1/projects/skia/issues"
)

// IssueTracker is a genric interface to an issue tracker that allows us
// to connect issues with items (identified by an id).
type IssueTracker interface {
	// FromQueury returns issue that match the given query string.
	FromQuery(q string) ([]Issue, error)
	// AddComment adds a comment to the issue with the given id
	AddComment(id string, comment CommentRequest) error
	// AddIssue creates an issue with the passed in params.
	AddIssue(issue IssueRequest) error
}

// Issue is an individual issue returned from the project hosting response.
type Issue struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	State string `json:"state"`
}

// IssueResponse is used to decode JSON responses from the project hosting API.
type IssueResponse struct {
	Items []Issue `json:"items"`
}

type CommentRequest struct {
	Content string `json:"content"`
}

type MonorailPerson struct {
	Name     string `json:"name"`     // Email address
	HtmlLink string `json:"htmlLink"` // Links to user id
	Kind     string `json:"kind"`     // Is always "monorail#issuePerson"
}

type IssueRequest struct {
	Status      string           `json:"status"`
	Owner       MonorailPerson   `json:"owner"`
	CC          []MonorailPerson `json:"cc"`
	Labels      []string         `json:"labels"`
	Summary     string           `json:"summary"`
	Description string           `json:"description"`
}

// MonorailIssueTracker implements IssueTracker.
//
// Note that to use a MonorailIssueTracker from GCE that client id of the
// project needs to be whitelisted in Monorail, which is already done for Skia
// Infra. Also note that the instance running needs to have the
// https://www.googleapis.com/auth/userinfo.email scope added to it.
type MonorailIssueTracker struct {
	client *http.Client
}

func NewMonorailIssueTracker(client *http.Client) IssueTracker {
	return &MonorailIssueTracker{
		client: client,
	}
}

// FromQuery is part of the IssueTracker interface. See documentation there.
func (m *MonorailIssueTracker) FromQuery(q string) ([]Issue, error) {
	query := url.Values{}
	query.Add("q", q)
	query.Add("fields", "items/id,items/state,items/title")
	return get(m.client, MONORAIL_BASE_URL+"?"+query.Encode())
}

// AddComment adds a comment to the issue with the given id
func (m *MonorailIssueTracker) AddComment(id string, comment CommentRequest) error {
	u := fmt.Sprintf("%s/%s/comments", MONORAIL_BASE_URL, id)
	return post(m.client, u, comment)
}

// AddIssue creates an issue with the passed in params.
func (m *MonorailIssueTracker) AddIssue(issue IssueRequest) error {
	return post(m.client, MONORAIL_BASE_URL, issue)
}

func get(client *http.Client, u string) ([]Issue, error) {
	resp, err := client.Get(u)
	if err != nil || resp == nil || resp.StatusCode != 200 {
		return nil, fmt.Errorf("Failed to retrieve issue tracker response: %s", err)
	}
	defer util.Close(resp.Body)

	issueResponse := &IssueResponse{
		Items: []Issue{},
	}
	if err := json.NewDecoder(resp.Body).Decode(&issueResponse); err != nil {
		return nil, err
	}

	return issueResponse.Items, nil
}

func post(client *http.Client, dst string, request interface{}) error {
	b := new(bytes.Buffer)
	e := json.NewEncoder(b)
	if err := e.Encode(request); err != nil {
		return fmt.Errorf("Problem encoding json for request: %s", err)
	}

	resp, err := client.Post(dst, "application/json", b)

	if err != nil || resp == nil || resp.StatusCode != 200 {
		return fmt.Errorf("Failed to retrieve issue tracker response: %s", err)
	}
	defer util.Close(resp.Body)
	msg, err := ioutil.ReadAll(resp.Body)
	glog.Infof("%s\n\nErr: %v", string(msg), err)
	return nil
}
