package main

import (
	"bufio"
	"fmt"
	"github.com/jasdel/harvester/internal/common"
	"github.com/jasdel/harvester/internal/queue"
	"github.com/jasdel/harvester/internal/storage"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
)

// Response message to a successful job being scheduled
type jobScheduledMsg struct {
	// Id of the scheduled job
	JobId common.JobId `json:"jobId"`
}

// Handles the request to schedule a new job. Expects a new line separated
// list of URLs as input in the request's body. Will respond back with error
// message, or job id if the schedule was successful.
//
// e.g:
// curl -X POST --data-binary @- "http://localhost:8080" << EOF
// https://www.google.com
// http://example.com
// EOF
//
// An optional 'forceCrawl' query parameter can be provided to
// force crawling of previously crawled URLs. This flag applies
// to add descendants of each Job URL being scheduled. The parameter
// doesn't take a value, but if one is provided it will be ignored.
// If the parameter is present the job's URLs will be crawled,
// ignoring the cache.
//
// Response:
//	- Success: {jobId: 1234}
//	- Failure: {code: <code>, message: <message>}
type JobScheduleHandler struct {
	urlQueuePub queue.Publisher
	sc          *storage.Client
}

func (h *JobScheduleHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		http.Error(w, "MethodNotAllowed", http.StatusMethodNotAllowed)
		return
	}

	forceCrawl := false
	if _, ok := r.URL.Query()["forceCrawl"]; ok {
		forceCrawl = true
	}

	urls, err := getRequestedJobURLs(r.Body)
	if err != nil {
		log.Println("routeScheduleJob request parse failed", err)
		writeJSONError(w, "BadRequest", err.Short(), http.StatusBadRequest)
		return
	}

	if len(urls) == 0 {
		// Nothing can be done if there are no URLs to schedule
		log.Println("routeScheduleJob request has no URLs")
		writeJSONError(w, "BadRequest", "No URLs provided", http.StatusBadRequest)
		return
	}

	// Create job by sending the URLs to scheduler
	id, err := h.scheduleJob(urls, forceCrawl)
	if err != nil {
		log.Println("routeScheduleJob request job schedule failed.", err)
		writeJSONError(w, "DependancyFailure", err.Short(), http.StatusInternalServerError)
		return
	}

	// Write job status out
	writeJSON(w, jobScheduledMsg{JobId: id}, http.StatusOK)
}

// Reads the input scanning for URLs. It expects a single URL per
// line. If there is a failure reading from the input, or a invalid
// URL is encountered an error will be returned.
func getRequestedJobURLs(in io.Reader) ([]string, *ErroMsg) {
	scanner := bufio.NewScanner(in)

	urlMap := make(map[string]struct{})
	urls := []string{}
	for scanner.Scan() {
		if scanner.Text() == "" {
			continue
		}

		u, err := validateJobURL(scanner.Text())
		if err != nil {
			return nil, &ErroMsg{
				Source: "getRequestedJobURLs",
				Info:   fmt.Sprintf("Invalid URL: %s", scanner.Text()),
				Err:    err,
			}
		}
		if _, ok := urlMap[u]; ok {
			continue
		}
		urlMap[u] = struct{}{}

		urls = append(urls, u)
	}
	if err := scanner.Err(); err != nil {
		return nil, &ErroMsg{
			Source: "getRequestedJobURLs",
			Info:   "Unexpected error in input",
			Err:    err,
		}
	}

	return urls, nil
}

// Validates the job URL contains at least a host and scheme. The scheme is also validated
// as being http or https. If no scheme is provided http will be used as the default.
func validateJobURL(jobURL string) (string, error) {
	if strings.HasPrefix(jobURL, "/") {
		return "", fmt.Errorf("Invalid URL, does not have host")
	}

	u, err := url.Parse(jobURL)
	if err != nil {
		return "", err
	}
	if u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("Invalid URL scheme")
	}
	if u.Scheme == "" {
		// set default scheme if non are provided, so the input could be www.example.com
		u.Scheme = "http"
	}
	return u.String(), nil
}

// Requests that a job be created, and the parts of it be scheduled.
// a job id will be returned if the job was successfully created, and
// error if there was a failure.
func (h *JobScheduleHandler) scheduleJob(urls []string, forceCrawl bool) (common.JobId, *ErroMsg) {
	job, err := h.sc.JobClient().CreateJobFromURLs(urls)
	if err != nil {
		return common.InvalidId, &ErroMsg{
			Source: "JobScheduleHandler.scheduleJob",
			Info:   fmt.Sprintf("Create Job Failed"),
			Err:    err,
		}
	}

	go func() {
		for _, u := range job.URLs {
			if err := h.sc.URLClient().AddPending(job.Id, u.URLId, u.URLId); err != nil {
				log.Println("JobScheduleHandler.scheduleJob: failed to add job URL to pending list", err)
			}
			h.urlQueuePub.Send(&common.URLQueueItem{
				JobId:      job.Id,
				OriginId:   u.URLId,
				URLId:      u.URLId,
				ReferId:    common.InvalidId,
				ForceCrawl: forceCrawl,
			})
		}
	}()

	return job.Id, nil
}
