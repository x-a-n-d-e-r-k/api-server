package handlers

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"time"

	"github.com/gorilla/context"
	"github.com/gorilla/mux"
	"github.com/lair-framework/api-server/app"
	"github.com/lair-framework/api-server/lib/ip"
	"github.com/lair-framework/api-server/middleware"
	"github.com/lair-framework/go-lair"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

const (
	// MAXPORTS is the maximum number of ports allowable for a single host without ForcePorts enabled.
	MAXPORTS = 1000
)

// Return the status as a string based off the cvss value
func calcRating(cvss float64) string {
	switch {
	case cvss >= 7:
		return "high"
	case cvss >= 4:
		return "medium"
	default:
		return "low"
	}
}

func removeDuplicates(in []string) []string {
	m := map[string]bool{}
	out := []string{}
	for _, i := range in {
		if _, ok := m[i]; ok {
			continue
		}
		m[i] = true
		out = append(out, i)
	}
	return out
}

// UpdateProject is an HTTP handler to add/update a project using additive, smart merge
func UpdateProject(server *app.App) func(w http.ResponseWriter, req *http.Request) {
	return func(w http.ResponseWriter, req *http.Request) {
		body, err := ioutil.ReadAll(req.Body)
		if err != nil {
			server.R.JSON(w, http.StatusInternalServerError, &app.Response{Status: "Error", Message: "Unable to read request body"})
			return
		}
		var doc lair.Project
		if err := json.Unmarshal(body, &doc); err != nil {
			server.R.JSON(w, http.StatusInternalServerError, &app.Response{Status: "Error", Message: err.Error()})
			return
		}

		db := context.Get(req, "db").(*mgo.Database)
		if db == nil {
			server.R.JSON(w, http.StatusInternalServerError, &app.Response{Status: "Error", Message: "Unable to connect to database"})
			return
		}

		forcePorts := false
		// Read 'force-ports' URL parameter
		vars := mux.Vars(req)
		forcePortsStr := vars["force-ports"]
		if forcePortsStr == "true" {
			forcePorts = true
		}

		// Start of import

		// Validate versions
		var v lair.Version
		if err := db.C(server.C.Versions).Find(nil).One(&v); err != nil || v.Value != server.Version {
			server.R.JSON(w, http.StatusInternalServerError, &app.Response{Status: "Error", Message: "Incompatible versions"})
			return
		}

		// Validate required fields
		if doc.ID == "" || doc.Commands == nil || len(doc.Commands) <= 0 {
			server.R.JSON(w, http.StatusInternalServerError, &app.Response{Status: "Error", Message: "Missing required field or invalid format"})
			return
		}

		// Lookup project
		var project lair.Project
		pid := doc.ID
		if err := db.C(server.C.Projects).FindId(doc.ID).One(&project); err != nil {
			server.R.JSON(w, http.StatusInternalServerError, &app.Response{Status: "Error", Message: "Invalid project id"})
			return
		}

		// Append new commands
		project.Commands = append(project.Commands, doc.Commands...)

		// Append new notes
		project.Notes = append(project.Notes, doc.Notes...)

		// Add owner if necessary
		if project.Owner == "" {
			project.Owner = doc.Owner
		}

		// Add industry if necessary
		if project.Industry == "" {
			project.Industry = doc.Industry
		}

		// Add creation date if necessary
		if project.CreatedAt == "" {
			project.CreatedAt = doc.CreatedAt
		}

		// Add description if necessary
		if project.Description == "" {
			project.Description = doc.Description
		}

		// Used for tracking any hosts that were skipped for exceeding MAXPORTS limit
		skippedHosts := map[string]bool{}

		// Ensure indexes
		db.C(server.C.Hosts).EnsureIndexKey("projectId", "ipv4")
		db.C(server.C.Services).EnsureIndexKey("projectId", "hostId", "port", "protocol")
		db.C(server.C.Issues).EnsureIndexKey("projectId", "pluginIds")
		db.C(server.C.WebDirectories).EnsureIndexKey("projectId", "hostId", "path", "port")

		// Insert auth interfaces
		for _, docAI := range doc.AuthInterfaces {
			ai := &lair.AuthInterface{
				ProjectID:     pid,
				IsMultifactor: docAI.IsMultifactor,
				Kind:          docAI.Kind,
				URL:           docAI.URL,
				Description:   docAI.Description,
			}
			db.C(server.C.AuthInterfaces).Insert(ai)
		}

		// Insert credentials
		for _, docCred := range doc.Credentials {
			cred := &lair.Credential{
				ProjectID: pid,
				Username:  docCred.Username,
				Password:  docCred.Password,
				Format:    docCred.Format,
				Hash:      docCred.Hash,
				Host:      docCred.Host,
				Service:   docCred.Service,
			}
			db.C(server.C.Credentials).Insert(cred)
		}

		// Insert People
		for _, docPerson := range doc.People {
			person := &lair.Person{
				ProjectID:         pid,
				PrincipalName:     docPerson.PrincipalName,
				SAMAccountName:    docPerson.SAMAccountName,
				DistinguishedName: docPerson.DistinguishedName,
				FirstName:         docPerson.FirstName,
				MiddleName:        docPerson.MiddleName,
				LastName:          docPerson.LastName,
				DisplayName:       docPerson.DisplayName,
				Department:        docPerson.Department,
				Description:       docPerson.Description,
				Address:           docPerson.Address,
				Emails:            docPerson.Emails,
				Phones:            docPerson.Phones,
				References:        docPerson.References,
				Groups:            docPerson.Groups,
				LastLogon:         docPerson.LastLogon,
				LastLogoff:        docPerson.LastLogoff,
				LoggedIn:          docPerson.LoggedIn,
			}
			db.C(server.C.People).Insert(person)
		}

		// Insert netblocks
		for _, docNetblock := range doc.Netblocks {
			netblock := &lair.Netblock{}
			knownNetblock := true
			// Determine if the netblock is already in database
			m := bson.M{"projectId": pid, "cidr": docNetblock.CIDR}
			if err := db.C(server.C.Netblocks).Find(m).One(&netblock); err != nil {
				knownNetblock = false
			}

			// Used for checking if the netblock has changed during import
			data := []byte(fmt.Sprintf("%+v", netblock))
			preMD5 := fmt.Sprintf("%x", md5.Sum(data))

			netblock.ProjectID = pid
			netblock.CIDR = docNetblock.CIDR

			if netblock.ASN == "" {
				netblock.ASN = docNetblock.ASN
			}

			if netblock.ASNCountryCode == "" {
				netblock.ASNCountryCode = docNetblock.ASNCountryCode
			}

			if netblock.ASNCIDR == "" {
				netblock.ASNCIDR = docNetblock.ASNCIDR
			}

			if netblock.ASNDate == "" {
				netblock.ASNDate = docNetblock.ASNDate
			}

			if netblock.ASNRegistry == "" {
				netblock.ASNRegistry = docNetblock.ASNRegistry
			}

			if netblock.AbuseEmails == "" {
				netblock.AbuseEmails = docNetblock.AbuseEmails
			}

			if netblock.MiscEmails == "" {
				netblock.MiscEmails = docNetblock.MiscEmails
			}

			if netblock.TechEmails == "" {
				netblock.TechEmails = docNetblock.TechEmails
			}

			if netblock.Name == "" {
				netblock.Name = docNetblock.Name
			}

			if netblock.City == "" {
				netblock.City = docNetblock.City
			}

			if netblock.Country == "" {
				netblock.Country = docNetblock.Country
			}

			if netblock.PostalCode == "" {
				netblock.PostalCode = docNetblock.PostalCode
			}

			if netblock.Created == "" {
				netblock.Created = docNetblock.Created
			}

			if netblock.Updated == "" {
				netblock.Updated = docNetblock.Updated
			}

			if netblock.Description == "" {
				netblock.Description = docNetblock.Description
			}

			if netblock.Handle == "" {
				netblock.Handle = docNetblock.Handle
			}

			if !knownNetblock {
				msg := fmt.Sprintf("%s - New netblock found: %s", time.Now().String(), docNetblock.CIDR)
				project.DroneLog = append(project.DroneLog, msg)
			}

			data = []byte(fmt.Sprintf("%+v", netblock))
			postMD5 := fmt.Sprintf("%x", md5.Sum(data))

			// Check if host was changed
			if preMD5 != postMD5 {
				if !knownNetblock {
					id := bson.NewObjectId().Hex()
					netblock.ID = id
				}

				// Upsert changes
				db.C(server.C.Netblocks).UpsertId(netblock.ID, netblock)
			}
		}

		// Procss the hosts
		for _, docHost := range doc.Hosts {
			if len(docHost.Services) > MAXPORTS && !forcePorts {
				// Host exceeds max number of allowable ports. Skip it.
				skippedHosts[docHost.IPv4] = true
				msg := fmt.Sprintf(
					"%s - Host skipped. Exceeded maximum number of ports: %s",
					time.Now().String(),
					docHost.IPv4,
				)
				project.DroneLog = append(project.DroneLog, msg)
				continue
			}
			host := &lair.Host{}
			knownHost := true
			// Determine if the host is already in database
			m := bson.M{"projectId": pid, "ipv4": docHost.IPv4}
			if err := db.C(server.C.Hosts).Find(m).One(&host); err != nil {
				knownHost = false
			}

			// Used for checking if the host values changed during import
			data := []byte(fmt.Sprintf("%+v", host))
			preMD5 := fmt.Sprintf("%x", md5.Sum(data))

			// Initialize basic host info
			host.ProjectID = pid
			host.IPv4 = docHost.IPv4
			host.LongIPv4Addr = ip.IpToInt(net.ParseIP(host.IPv4))

			if host.MAC == "" {
				host.MAC = docHost.MAC
			}

			// Append all host notes
			host.Notes = append(host.Notes, docHost.Notes...)
			// Append all tags
			host.Tags = removeDuplicates(append(host.Tags, docHost.Tags...))

			// Add any new files
			for idx, docFile := range docHost.Files {
				knownFile := false
				for k, f := range host.Files {
					if docFile.FileName == f.FileName {
						// File exists, update URL
						knownFile = true
						host.Files[k].URL = docFile.URL
						break
					}
				}
				if !knownFile {
					host.Files = append(host.Files, docHost.Files[idx])
				}
			}

			// Add any new hostnames
			for _, docHostname := range docHost.Hostnames {
				found := false
				for _, dbHostname := range host.Hostnames {
					if dbHostname == docHostname {
						found = true
					}
				}
				if !found {
					host.Hostnames = append(host.Hostnames, docHostname)
					host.LastModifiedBy = doc.Tool
				}
			}

			// Add any new OS
			if host.OS.Weight < docHost.OS.Weight {
				host.OS = docHost.OS
				host.LastModifiedBy = doc.Tool
			}

			data = []byte(fmt.Sprintf("%+v", host))
			postMD5 := fmt.Sprintf("%x", md5.Sum(data))

			// Check if host was changed
			if preMD5 != postMD5 {
				host.LastModifiedBy = doc.Tool
				if !knownHost {
					id := bson.NewObjectId().Hex()
					host.ID = id
					host.Status = docHost.Status
					if !server.IsValidStatus(docHost.Status) {
						host.Status = lair.StatusGrey
					}
				}

				// Upsert changes
				db.C(server.C.Hosts).UpsertId(host.ID, host)

			}

			if !knownHost {
				msg := fmt.Sprintf("%s - New host found: %s", time.Now().String(), docHost.IPv4)
				project.DroneLog = append(project.DroneLog, msg)
			}

			// Process web directories
			for _, docDir := range docHost.WebDirectories {
				m := bson.M{
					"projectId": pid,
					"hostId":    host.ID,
					"path":      docDir.Path,
					"port":      docDir.Port,
				}
				// Determine if the web directory is already in database
				webDir := &lair.WebDirectory{}
				if err := db.C(server.C.WebDirectories).Find(m).One(&webDir); err != nil {
					// Web directory doesn't exist, create a new one
					webDir.ID = bson.NewObjectId().Hex()
					webDir.ProjectID = pid
					webDir.HostID = host.ID
					webDir.Path = docDir.Path
					webDir.Port = docDir.Port
					webDir.ResponseCode = docDir.ResponseCode
					webDir.LastModifiedBy = docDir.LastModifiedBy
					webDir.IsFlagged = docDir.IsFlagged
				} else {
					// Web directory exists in database, update relevant fields
					webDir.ResponseCode = docDir.ResponseCode
					webDir.LastModifiedBy = docDir.LastModifiedBy
					webDir.IsFlagged = docDir.IsFlagged
				}

				// Upsert changes
				db.C(server.C.WebDirectories).UpsertId(webDir.ID, webDir)
			}

			for _, docService := range docHost.Services {

				m := bson.M{
					"projectId": pid,
					"hostId":    host.ID,
					"port":      docService.Port,
					"protocol":  docService.Protocol,
				}
				// Determine if the host is already in database
				service := &lair.Service{}
				knownPort := true
				if err := db.C(server.C.Services).Find(m).One(&service); err != nil {
					knownPort = false
				}

				// Used for tracking if changes were made to service
				data = []byte(fmt.Sprintf("%+v", service))
				preMD5 := fmt.Sprintf("%x", md5.Sum(data))

				service.HostID = host.ID
				service.ProjectID = pid
				service.Protocol = docService.Protocol
				service.Port = docService.Port

				if service.Product == "" || service.Product == "Unknown" {
					service.Product = docService.Product
				}

				if service.Service == "" || service.Service == "Unknown" {
					service.Service = docService.Service
				}

				// Append all service notes
				service.Notes = append(service.Notes, docService.Notes...)

				// Add any new files
				for idx, docFile := range docService.Files {
					knownFile := false
					for k, f := range service.Files {
						if docFile.FileName == f.FileName {
							// File exists, update URL
							knownFile = true
							service.Files[k].URL = docFile.URL
							break
						}
					}
					if !knownFile {
						service.Files = append(service.Files, docService.Files[idx])
					}
				}

				if !knownPort {
					id := bson.NewObjectId().Hex()
					service.ID = id
					service.Status = docService.Status
					if !server.IsValidStatus(service.Status) {
						service.Status = lair.StatusGrey
					}
					msg := fmt.Sprintf(
						"%s - New service found: %d/%s (%s)",
						time.Now().String(),
						docService.Port,
						docService.Protocol,
						docService.Service,
					)
					project.DroneLog = append(project.DroneLog, msg)
				}

				// Used for tracking if changes were made to service
				data = []byte(fmt.Sprintf("%+v", service))
				postMD5 = fmt.Sprintf("%x", md5.Sum(data))

				// Upsert any changes
				if preMD5 != postMD5 {
					service.LastModifiedBy = doc.Tool
					db.C(server.C.Services).UpsertId(service.ID, service)
				}
			}
		}

		for _, docIssue := range doc.Issues {
			pluginM := bson.M{
				"$all": docIssue.PluginIDs,
			}
			m := bson.M{
				"project_id": pid,
				"plugin_ids": pluginM,
			}
			// Check if vulnerability already exists
			issue := &lair.Issue{}
			knownIssue := true
			if err := db.C(server.C.Issues).Find(m).One(&issue); err != nil {
				knownIssue = false
			}

			if !knownIssue {
				hostList := []lair.IssueHost{}
				// Build a list of hosts NOT marked as 'skipped' meaning they didn't exceed
				// port count limit.
				for idx, host := range docIssue.Hosts {
					if _, skipped := skippedHosts[host.IPv4]; !skipped {
						hostList = append(hostList, docIssue.Hosts[idx])
					}
				}
				id := bson.NewObjectId().Hex()
				issue.ID = id
				issue.ProjectID = pid
				issue.Title = docIssue.Title
				issue.Description = docIssue.Description
				issue.Solution = docIssue.Solution
				issue.Evidence = docIssue.Evidence
				issue.CVSS = docIssue.CVSS
				issue.Rating = calcRating(issue.CVSS)
				issue.IsConfirmed = docIssue.IsConfirmed
				issue.IsFlagged = docIssue.IsFlagged
				issue.LastModifiedBy = doc.Tool
				issue.IdentifiedBy = []string{doc.Tool}
				issue.Status = docIssue.Status
				issue.Files = append(issue.Files, docIssue.Files...)
				if !server.IsValidStatus(issue.Status) {
					issue.Status = lair.StatusGrey
				}
				issue.PluginIDs = docIssue.PluginIDs
				issue.CVEs = docIssue.CVEs
				issue.Notes = docIssue.Notes
				issue.Hosts = hostList
				msg := fmt.Sprintf(
					"%s - New issue found: %s",
					time.Now().String(),
					docIssue.Title,
				)
				project.DroneLog = append(project.DroneLog, msg)

				// Insert new vulnerability
				if err := db.C(server.C.Issues).Insert(issue); err != nil {
					// TODO: How to handle failed issue insert?
				}
			}

			if knownIssue {

				// Used for tracking if changes were made to issueerability
				data := []byte(fmt.Sprintf("%+v", issue))
				preMD5 := fmt.Sprintf("%x", md5.Sum(data))

				issue.Title = docIssue.Title
				issue.Description = docIssue.Description
				issue.Solution = docIssue.Solution
				if issue.Evidence != docIssue.Evidence {
					issue.Evidence = issue.Evidence + "\n\n" + docIssue.Evidence
				}

				// Add any new CVEs
				for _, docCVE := range docIssue.CVEs {
					found := false
					for _, dbCVE := range issue.CVEs {
						if dbCVE == docCVE {
							found = true
						}
					}
					if !found {
						issue.CVEs = append(issue.CVEs, docCVE)
						issue.LastModifiedBy = doc.Tool
					}
				}

				// Add any new files
				for idx, docFile := range docIssue.Files {
					knownFile := false
					for k, f := range issue.Files {
						if docFile.FileName == f.FileName {
							// File exists, update URL
							knownFile = true
							issue.Files[k].URL = docFile.URL
							break
						}
					}
					if !knownFile {
						issue.Files = append(issue.Files, docIssue.Files[idx])
					}
				}

				// Add any new hosts
				for _, hk := range docIssue.Hosts {
					if _, skipped := skippedHosts[hk.IPv4]; skipped {
						// Host is marked as skipped, meaning it exceeded port limit. Do not process it.
						continue
					}
					found := false
					for _, dbHk := range issue.Hosts {
						if dbHk.IPv4 == hk.IPv4 && dbHk.Port == hk.Port && dbHk.Protocol == hk.Protocol {
							found = true
						}
					}
					if !found {
						issue.Hosts = append(issue.Hosts, hk)
						issue.LastModifiedBy = doc.Tool
						msg := fmt.Sprintf(
							"%s - %s:%d/%s - New issue found: %s",
							time.Now().String(),
							hk.IPv4,
							hk.Port,
							hk.Protocol,
							docIssue.Title,
						)
						project.DroneLog = append(project.DroneLog, msg)
					}
				}

				// Add any new plugins
				for _, docPlugin := range docIssue.PluginIDs {
					found := false
					for _, dbPlugin := range issue.PluginIDs {
						if dbPlugin.Tool == docPlugin.Tool && dbPlugin.ID == docPlugin.ID {
							found = true
						}
					}
					if !found {
						issue.PluginIDs = append(issue.PluginIDs, docPlugin)
						issue.LastModifiedBy = doc.Tool
					}
				}

				// Append notes
				issue.Notes = append(issue.Notes, docIssue.Notes...)

				// Add any new 'Identified By' info
				found := false
				for _, idBy := range issue.IdentifiedBy {
					if idBy == doc.Tool {
						found = true
					}
				}
				if !found {
					issue.IdentifiedBy = append(issue.IdentifiedBy, doc.Tool)
					issue.LastModifiedBy = doc.Tool
				}

				// Only set flag to 'true', don't unset it
				if docIssue.IsFlagged {
					issue.IsFlagged = true
				}

				// Only set confirmed to 'true', don't unset it
				if docIssue.IsConfirmed {
					issue.IsConfirmed = true
				}

				// Check if issue data was changed
				data = []byte(fmt.Sprintf("%+v", issue))
				postMD5 := fmt.Sprintf("%x", md5.Sum(data))

				if preMD5 != postMD5 {
					// Upsert changes
					issue.LastModifiedBy = doc.Tool
					db.C(server.C.Issues).UpsertId(issue.ID, issue)
				}
			}
		}

		// Ensure the correct drone log hisory size is maintained
		if len(project.DroneLog) > server.History {
			project.DroneLog = project.DroneLog[len(project.DroneLog)-server.History:]
		}

		// Update project
		db.C(server.C.Projects).UpdateId(project.ID, project)

		// End of import

		server.R.JSON(w, http.StatusOK, &app.Response{Status: "Ok"})
	}
}

// ShowProject is an HTTP handler to retrieve a single project.
func ShowProject(server *app.App) func(w http.ResponseWriter, req *http.Request) {
	return func(w http.ResponseWriter, req *http.Request) {
		db := context.Get(req, "db").(*mgo.Database)
		if db == nil {
			server.R.JSON(w, http.StatusInternalServerError, &app.Response{Status: "Error", Message: "Unable to access database"})
			return
		}
		vars := mux.Vars(req)
		pid := vars["pid"]
		project := &lair.Project{}
		if err := db.C(server.C.Projects).FindId(pid).One(&project); err != nil {
			server.R.JSON(w, http.StatusInternalServerError, &app.Response{Status: "Error", Message: "Unable to retrieve project or project does not exist"})
			return
		}
		server.R.JSON(w, http.StatusOK, project)
	}
}

// IndexProject is an HTTP handler to retrieve a list of all projects that a user owns or is a contributor for.
func IndexProject(server *app.App) func(w http.ResponseWriter, req *http.Request) {
	return func(w http.ResponseWriter, req *http.Request) {
		db := context.Get(req, "db").(*mgo.Database)
		if db == nil {
			server.R.JSON(w, http.StatusInternalServerError, &app.Response{Status: "Error", Message: "Unable to access database"})
			return
		}

		user := context.Get(req, "user").(*middleware.User)
		if user == nil {
			server.R.JSON(w, http.StatusInternalServerError, &app.Response{Status: "Error", Message: "Unable to retrieve user"})
			return
		}

		// Ensure query is restricted to only projects to which the user is authorized
		or := &bson.M{
			"$or": []bson.M{
				bson.M{"owner": user.Id},
				bson.M{"contributors": user.Id},
			},
		}
		var projects []lair.Project
		if err := db.C(server.C.Projects).Find(or).All(&projects); err != nil {
			server.R.JSON(w, http.StatusInternalServerError, &app.Response{Status: "Error", Message: "Unable to retrieve project index"})
			return
		}
		server.R.JSON(w, http.StatusOK, projects)
	}
}
