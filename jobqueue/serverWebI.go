// Copyright © 2016-2017 Genome Research Limited
// Author: Sendu Bala <sb10@sanger.ac.uk>.
//
//  This file is part of wr.
//
//  wr is free software: you can redistribute it and/or modify
//  it under the terms of the GNU Lesser General Public License as published by
//  the Free Software Foundation, either version 3 of the License, or
//  (at your option) any later version.
//
//  wr is distributed in the hope that it will be useful,
//  but WITHOUT ANY WARRANTY; without even the implied warranty of
//  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
//  GNU Lesser General Public License for more details.
//
//  You should have received a copy of the GNU Lesser General Public License
//  along with wr. If not, see <http://www.gnu.org/licenses/>.

package jobqueue

// This file contains the web interface code of the server.

import (
	"github.com/VertebrateResequencing/wr/queue"
	"github.com/gorilla/websocket"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
)

// jstatusReq is what the status webpage sends us to ask for info about jobs.
// The possible Requests are:
// current = get count info for every job in every RepGroup in the cmds queue.
// details = get example job details for jobs in the RepGroup, grouped by having
//           the same Status, Exitcode and FailReason.
// retry = retry the buried jobs with the given RepGroup, ExitCode and FailReason.
// kill = kill the running jobs with the given RepGroup.
// confirmBadServer = confirm that the server with ID ServerID is bad.
// dismissMsg = dismiss the given Msg.
type jstatusReq struct {
	Key        string   // sending Key means "give me detailed info about this single job"
	RepGroup   string   // sending RepGroup means "send me limited info about the jobs with this RepGroup"
	State      JobState // A Job.State to limit RepGroup by
	Exitcode   int
	FailReason string
	All        bool // If false, retry mode will act on a single random matching job, instead of all of them
	ServerID   string
	Msg        string
	Request    string
}

// jstatus is the job info we send to the status webpage (only real difference
// to Job is that some of the values are converted to easy-to-display forms).
type jstatus struct {
	Key          string
	RepGroup     string
	DepGroups    []string
	Dependencies []string
	Cmd          string
	State        JobState
	Cwd          string
	CwdBase      string
	HomeChanged  bool
	Behaviours   string
	Mounts       string
	// ExpectedRAM is in Megabytes.
	ExpectedRAM int
	// ExpectedTime is in seconds.
	ExpectedTime float64
	// RequestedDisk is in Gigabytes.
	RequestedDisk int
	Cores         int
	PeakRAM       int
	Exited        bool
	Exitcode      int
	FailReason    string
	Pid           int
	Host          string
	HostID        string
	HostIP        string
	Walltime      float64
	CPUtime       float64
	Started       int64
	Ended         int64
	StdErr        string
	StdOut        string
	// Env        []string //*** not sending Env until we have https implemented
	Attempts uint32
	Similar  int
}

// webInterfaceStatic is a http handler for our static documents in static.go
// (which in turn come from the static folder in the git repository). static.go
// is auto-generated by:
// $ esc -pkg jobqueue -prefix static -private -o jobqueue/static.go static
func webInterfaceStatic(w http.ResponseWriter, r *http.Request) {
	// our home page is /status.html
	path := r.URL.Path
	if path == "/" || path == "/status" {
		path = "/status.html"
	}

	// during development, to avoid having to rebuild and restart manager on
	// every change to a file in static dir, do:
	// $ esc -pkg jobqueue -prefix $GOPATH/src/github.com/VertebrateResequencing/wr/static -private -o jobqueue/static.go $GOPATH/src/github.com/VertebrateResequencing/wr/static
	// and set the boolean to true. Don't forget to rerun esc without the abs
	// paths and change the boolean back to false before any commit!
	doc, err := _escFSByte(false, path)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if strings.HasPrefix(path, "/js") {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	} else if strings.HasPrefix(path, "/css") {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	} else if strings.HasPrefix(path, "/fonts") {
		if strings.HasSuffix(path, ".eot") {
			w.Header().Set("Content-Type", "application/vnd.ms-fontobject")
		} else if strings.HasSuffix(path, ".svg") {
			w.Header().Set("Content-Type", "image/svg+xml")
		} else if strings.HasSuffix(path, ".ttf") {
			w.Header().Set("Content-Type", "application/x-font-truetype")
		} else if strings.HasSuffix(path, ".woff") {
			w.Header().Set("Content-Type", "application/font-woff")
		} else if strings.HasSuffix(path, ".woff2") {
			w.Header().Set("Content-Type", "application/font-woff2")
		}
	} else if strings.HasSuffix(path, "favicon.ico") {
		w.Header().Set("Content-Type", "image/x-icon")
	}

	w.Write(doc)
}

// webSocket upgrades a http connection to a websocket
func webSocket(w http.ResponseWriter, r *http.Request) (conn *websocket.Conn, ok bool) {
	var upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	ok = true
	if err != nil {
		http.Error(w, "Could not open websocket connection", http.StatusBadRequest)
		ok = false
	}
	return
}

// webInterfaceStatusWS reads from and writes to the websocket on the status
// webpage
func webInterfaceStatusWS(s *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, ok := webSocket(w, r)
		if !ok {
			log.Println("failed to set up websocket at", r.Host)
			return
		}

		writeMutex := &sync.Mutex{}

		// go routine to read client requests and respond to them
		go func(conn *websocket.Conn) {
			// log panics and die
			defer s.logPanic("jobqueue websocket client handling", true)

			for {
				req := jstatusReq{}
				err := conn.ReadJSON(&req)
				if err != nil { // probably the browser was refreshed, breaking conn
					break
				}

				q, existed := s.qs["cmds"]
				if !existed {
					continue
				}

				switch {
				case req.Key != "":
					jobs, _, errstr := s.getJobsByKeys(q, []string{req.Key}, true, true)
					if errstr == "" && len(jobs) == 1 {
						status := jobToStatus(jobs[0])
						writeMutex.Lock()
						err = conn.WriteJSON(status)
						writeMutex.Unlock()
						if err != nil {
							break
						}
					}
				case req.Request != "":
					switch req.Request {
					case "current":
						// get all current jobs
						jobs := s.getJobsCurrent(q, 0, "", false, false)
						writeMutex.Lock()
						err := webInterfaceStatusSendGroupStateCount(conn, "+all+", jobs)
						if err != nil {
							writeMutex.Unlock()
							break
						}

						// for each different RepGroup amongst these jobs,
						// send the job state counts
						repGroups := make(map[string][]*Job)
						for _, job := range jobs {
							repGroups[job.RepGroup] = append(repGroups[job.RepGroup], job)
						}
						failed := false
						for repGroup, jobs := range repGroups {
							complete, _, qerr := s.getCompleteJobsByRepGroup(repGroup)
							if qerr != "" {
								failed = true
								break
							}
							jobs = append(jobs, complete...)
							err := webInterfaceStatusSendGroupStateCount(conn, repGroup, jobs)
							if err != nil {
								failed = true
								break
							}
						}

						// also send details of dead servers
						for _, bs := range s.getBadServers() {
							s.badServerCaster.Send(bs)
						}

						// and of scheduler messages
						s.simutex.RLock()
						for _, si := range s.schedIssues {
							s.schedCaster.Send(si)
						}
						s.simutex.RUnlock()

						writeMutex.Unlock()
						if failed {
							break
						}
					case "details":
						// *** probably want to take the count as a req option,
						// so user can request to see more than just 1 job per
						// State+Exitcode+FailReason
						jobs, _, errstr := s.getJobsByRepGroup(q, req.RepGroup, 1, req.State, true, true)
						if errstr == "" && len(jobs) > 0 {
							writeMutex.Lock()
							failed := false
							for _, job := range jobs {
								status := jobToStatus(job)
								status.RepGroup = req.RepGroup // since we want to return the group the user asked for, not the most recent group the job was made for
								err = conn.WriteJSON(status)
								if err != nil {
									failed = true
									break
								}
							}
							writeMutex.Unlock()
							if failed {
								break
							}
						}
					case "retry":
						s.rpl.RLock()
						for key := range s.rpl.lookup[req.RepGroup] {
							item, err := q.Get(key)
							if err != nil {
								break
							}
							stats := item.Stats()
							if stats.State == queue.ItemStateBury {
								job := item.Data.(*Job)
								if job.Exitcode == req.Exitcode && job.FailReason == req.FailReason {
									err := q.Kick(key)
									if err != nil {
										break
									}
									job.UntilBuried = job.Retries + 1
									if !req.All {
										break
									}
								}
							}
						}
						s.rpl.RUnlock()
					case "remove":
						s.rpl.RLock()
						var toDelete []string
						for key := range s.rpl.lookup[req.RepGroup] {
							item, err := q.Get(key)
							if err != nil {
								break
							}
							stats := item.Stats()
							if stats.State == queue.ItemStateBury || stats.State == queue.ItemStateDelay || stats.State == queue.ItemStateDependent || stats.State == queue.ItemStateReady {
								job := item.Data.(*Job)
								if job.Exitcode == req.Exitcode && job.FailReason == req.FailReason {
									// we can't allow the removal of jobs that
									// have dependencies, as *queue would regard
									// that as satisfying the dependency and
									// downstream jobs would start
									hasDeps, err := q.HasDependents(key)
									if err != nil || hasDeps {
										continue
									}

									err = q.Remove(key)
									if err != nil {
										break
									}
									if err == nil {
										s.db.deleteLiveJob(key)
										toDelete = append(toDelete, key)
										if stats.State == queue.ItemStateDelay || stats.State == queue.ItemStateReady {
											s.decrementGroupCount(job.schedulerGroup, q)
										}
									}
									if !req.All {
										break
									}
								}
							}
						}
						for _, key := range toDelete {
							delete(s.rpl.lookup[req.RepGroup], key)
						}
						s.rpl.RUnlock()
					case "kill":
						s.rpl.RLock()
						for key := range s.rpl.lookup[req.RepGroup] {
							s.killJob(q, key)
						}
						s.rpl.RUnlock()
					case "confirmBadServer":
						if req.ServerID != "" {
							s.bsmutex.Lock()
							server := s.badServers[req.ServerID]
							delete(s.badServers, req.ServerID)
							s.bsmutex.Unlock()
							if server != nil && server.IsBad() {
								server.Destroy()
							}
						}
					case "dismissMsg":
						if req.Msg != "" {
							s.simutex.Lock()
							delete(s.schedIssues, req.Msg)
							s.simutex.Unlock()
						}
					default:
						continue
					}
				default:
					continue
				}
			}
		}(conn)

		// go routines to push changes to the client
		go func(conn *websocket.Conn) {
			// log panics and die
			defer s.logPanic("jobqueue websocket status updating", true)

			statusReceiver := s.statusCaster.Join()
			for status := range statusReceiver.In {
				writeMutex.Lock()
				err := conn.WriteJSON(status)
				writeMutex.Unlock()
				if err != nil {
					break
				}
			}
			statusReceiver.Close()
		}(conn)

		go func(conn *websocket.Conn) {
			defer s.logPanic("jobqueue websocket bad server updating", true)
			badserverReceiver := s.badServerCaster.Join()
			for server := range badserverReceiver.In {
				writeMutex.Lock()
				err := conn.WriteJSON(server)
				writeMutex.Unlock()
				if err != nil {
					break
				}
			}
			badserverReceiver.Close()
		}(conn)

		go func(conn *websocket.Conn) {
			defer s.logPanic("jobqueue websocket scheduler issue updating", true)
			schedIssueReceiver := s.schedCaster.Join()
			for si := range schedIssueReceiver.In {
				writeMutex.Lock()
				err := conn.WriteJSON(si)
				writeMutex.Unlock()
				if err != nil {
					break
				}
			}
			schedIssueReceiver.Close()
		}(conn)
	}
}

func jobToStatus(job *Job) jstatus {
	stderr, _ := job.StdErr()
	stdout, _ := job.StdOut()
	// env, _ := job.Env()
	var cwdLeaf string
	job.RLock()
	defer job.RUnlock()
	if job.ActualCwd != "" {
		cwdLeaf, _ = filepath.Rel(job.Cwd, job.ActualCwd)
		cwdLeaf = "/" + cwdLeaf
	}
	state := job.State
	if state == JobStateRunning && job.Lost {
		state = JobStateLost
	}
	return jstatus{
		Key:           job.key(),
		RepGroup:      job.RepGroup,
		DepGroups:     job.DepGroups,
		Dependencies:  job.Dependencies.Stringify(),
		Cmd:           job.Cmd,
		State:         state,
		CwdBase:       job.Cwd,
		Cwd:           cwdLeaf,
		HomeChanged:   job.ChangeHome,
		Behaviours:    job.Behaviours.String(),
		Mounts:        job.MountConfigs.String(),
		ExpectedRAM:   job.Requirements.RAM,
		ExpectedTime:  job.Requirements.Time.Seconds(),
		RequestedDisk: job.Requirements.Disk,
		Cores:         job.Requirements.Cores,
		PeakRAM:       job.PeakRAM,
		Exited:        job.Exited,
		Exitcode:      job.Exitcode,
		FailReason:    job.FailReason,
		Pid:           job.Pid,
		Host:          job.Host,
		HostID:        job.HostID,
		HostIP:        job.HostIP,
		Walltime:      job.WallTime().Seconds(),
		CPUtime:       job.CPUtime.Seconds(),
		Started:       job.StartTime.Unix(),
		Ended:         job.EndTime.Unix(),
		Attempts:      job.Attempts,
		Similar:       job.Similar,
		StdErr:        stderr,
		StdOut:        stdout,
		// Env:           env,
	}
}

// webInterfaceStatusSendGroupStateCount sends the per-repgroup state counts
// to the status webpage websocket
func webInterfaceStatusSendGroupStateCount(conn *websocket.Conn, repGroup string, jobs []*Job) (err error) {
	stateCounts := make(map[JobState]int)
	for _, job := range jobs {
		var state JobState

		// for display simplicity purposes, merge reserved in to running
		switch job.State {
		case JobStateReserved, JobStateRunning:
			state = JobStateRunning
		default:
			state = job.State
		}

		stateCounts[state]++
	}
	for to, count := range stateCounts {
		err = conn.WriteJSON(&jstateCount{repGroup, JobStateNew, to, count})
		if err != nil {
			return
		}
	}
	return
}
