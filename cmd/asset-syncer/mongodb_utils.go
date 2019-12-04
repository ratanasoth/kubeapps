/*
Copyright (c) 2018 The Helm Authors

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

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/disintegration/imaging"
	"github.com/globalsign/mgo/bson"
	"github.com/kubeapps/common/datastore"
	log "github.com/sirupsen/logrus"
)

const (
	chartCollection      = "charts"
	repositoryCollection = "repos"
	chartFilesCollection = "files"
)

type mongodbAssetManager struct {
	mongoConfig datastore.Config
	dbSession   datastore.Session
}

func newMongoDBManager(config datastore.Config) assetManager {
	return &mongodbAssetManager{config, nil}
}

func (m *mongodbAssetManager) Init() error {
	dbSession, err := datastore.NewSession(m.mongoConfig)
	if err != nil {
		return fmt.Errorf("Can't connect to mongoDB: %v", err)
	}
	m.dbSession = dbSession
	return nil
}

func (m *mongodbAssetManager) Close() error {
	return nil
}

// Syncing is performed in the following steps:
// 1. Update database to match chart metadata from index
// 2. Concurrently process icons for charts (concurrently)
// 3. Concurrently process the README and values.yaml for the latest chart version of each chart
// 4. Concurrently process READMEs and values.yaml for historic chart versions
//
// These steps are processed in this way to ensure relevant chart data is
// imported into the database as fast as possible. E.g. we want all icons for
// charts before fetching readmes for each chart and version pair.
func (m *mongodbAssetManager) Sync(charts []chart) error {
	err := m.importCharts(charts)
	if err != nil {
		return err
	}
	m.fetchFiles(charts)
	return nil
}

func (m *mongodbAssetManager) fetchFiles(charts []chart) {
	// Process 10 charts at a time
	numWorkers := 10
	iconJobs := make(chan chart, numWorkers)
	chartFilesJobs := make(chan importChartFilesJob, numWorkers)
	var wg sync.WaitGroup

	log.Debugf("starting %d workers", numWorkers)
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go m.importWorker(&wg, iconJobs, chartFilesJobs)
	}

	// Enqueue jobs to process chart icons
	for _, c := range charts {
		iconJobs <- c
	}
	// Close the iconJobs channel to signal the worker pools to move on to the
	// chart files jobs
	close(iconJobs)

	// Iterate through the list of charts and enqueue the latest chart version to
	// be processed. Append the rest of the chart versions to a list to be
	// enqueued later
	var toEnqueue []importChartFilesJob
	for _, c := range charts {
		chartFilesJobs <- importChartFilesJob{c.Name, c.Repo, c.ChartVersions[0]}
		for _, cv := range c.ChartVersions[1:] {
			toEnqueue = append(toEnqueue, importChartFilesJob{c.Name, c.Repo, cv})
		}
	}

	// Enqueue all the remaining chart versions
	for _, cfj := range toEnqueue {
		chartFilesJobs <- cfj
	}
	// Close the chartFilesJobs channel to signal the worker pools that there are
	// no more jobs to process
	close(chartFilesJobs)

	// Wait for the worker pools to finish processing
	wg.Wait()
}

func (m *mongodbAssetManager) RepoAlreadyProcessed(repoName string, checksum string) bool {
	db, closer := m.dbSession.DB()
	defer closer()
	lastCheck := &repoCheck{}
	err := db.C(repositoryCollection).Find(bson.M{"_id": repoName}).One(lastCheck)
	return err == nil && checksum == lastCheck.Checksum
}

func (m *mongodbAssetManager) UpdateLastCheck(repoName string, checksum string, now time.Time) error {
	db, closer := m.dbSession.DB()
	defer closer()
	_, err := db.C(repositoryCollection).UpsertId(repoName, bson.M{"$set": bson.M{"last_update": now, "checksum": checksum}})
	return err
}

func (m *mongodbAssetManager) Delete(repoName string) error {
	db, closer := m.dbSession.DB()
	defer closer()
	_, err := db.C(chartCollection).RemoveAll(bson.M{
		"repo.name": repoName,
	})
	if err != nil {
		return err
	}

	_, err = db.C(chartFilesCollection).RemoveAll(bson.M{
		"repo.name": repoName,
	})
	if err != nil {
		return err
	}

	_, err = db.C(repositoryCollection).RemoveAll(bson.M{
		"_id": repoName,
	})
	return err
}

func (m *mongodbAssetManager) importCharts(charts []chart) error {
	var pairs []interface{}
	var chartIDs []string
	for _, c := range charts {
		chartIDs = append(chartIDs, c.ID)
		// charts to upsert - pair of selector, chart
		pairs = append(pairs, bson.M{"_id": c.ID}, c)
	}

	db, closer := m.dbSession.DB()
	defer closer()
	bulk := db.C(chartCollection).Bulk()

	// Upsert pairs of selectors, charts
	bulk.Upsert(pairs...)

	// Remove charts no longer existing in index
	bulk.RemoveAll(bson.M{
		"_id": bson.M{
			"$nin": chartIDs,
		},
		"repo.name": charts[0].Repo.Name,
	})

	_, err := bulk.Run()
	return err
}

func (m *mongodbAssetManager) importWorker(wg *sync.WaitGroup, icons <-chan chart, chartFiles <-chan importChartFilesJob) {
	defer wg.Done()
	for c := range icons {
		log.WithFields(log.Fields{"name": c.Name}).Debug("importing icon")
		if err := m.fetchAndImportIcon(c); err != nil {
			log.WithFields(log.Fields{"name": c.Name}).WithError(err).Error("failed to import icon")
		}
	}
	for j := range chartFiles {
		log.WithFields(log.Fields{"name": j.Name, "version": j.ChartVersion.Version}).Debug("importing readme and values")
		if err := m.fetchAndImportFiles(j.Name, j.Repo, j.ChartVersion); err != nil {
			log.WithFields(log.Fields{"name": j.Name, "version": j.ChartVersion.Version}).WithError(err).Error("failed to import files")
		}
	}
}

func (m *mongodbAssetManager) fetchAndImportIcon(c chart) error {
	if c.Icon == "" {
		log.WithFields(log.Fields{"name": c.Name}).Info("icon not found")
		return nil
	}

	req, err := http.NewRequest("GET", c.Icon, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent())
	if len(c.Repo.AuthorizationHeader) > 0 {
		req.Header.Set("Authorization", c.Repo.AuthorizationHeader)
	}

	res, err := netClient.Do(req)
	if res != nil {
		defer res.Body.Close()
	}
	if err != nil {
		return err
	}

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%d %s", res.StatusCode, c.Icon)
	}

	b := []byte{}
	contentType := ""
	if strings.Contains(res.Header.Get("Content-Type"), "image/svg") {
		// if the icon is a SVG file simply read it
		b, err = ioutil.ReadAll(res.Body)
		if err != nil {
			return err
		}
		contentType = res.Header.Get("Content-Type")
	} else {
		// if the icon is in any other format try to convert it to PNG
		orig, err := imaging.Decode(res.Body)
		if err != nil {
			log.WithFields(log.Fields{"name": c.Name}).WithError(err).Error("failed to decode icon")
			return err
		}

		// TODO: make this configurable?
		icon := imaging.Fit(orig, 160, 160, imaging.Lanczos)

		var buf bytes.Buffer
		imaging.Encode(&buf, icon, imaging.PNG)
		b = buf.Bytes()
		contentType = "image/png"
	}

	db, closer := m.dbSession.DB()
	defer closer()
	return db.C(chartCollection).UpdateId(c.ID, bson.M{"$set": bson.M{"raw_icon": b, "icon_content_type": contentType}})
}

func (m *mongodbAssetManager) fetchAndImportFiles(name string, r *repo, cv chartVersion) error {
	chartFilesID := fmt.Sprintf("%s/%s-%s", r.Name, name, cv.Version)
	db, closer := m.dbSession.DB()
	defer closer()

	// Check if we already have indexed files for this chart version and digest
	if err := db.C(chartFilesCollection).Find(bson.M{"_id": chartFilesID, "digest": cv.Digest}).One(&chartFiles{}); err == nil {
		log.WithFields(log.Fields{"name": name, "version": cv.Version}).Debug("skipping existing files")
		return nil
	}
	log.WithFields(log.Fields{"name": name, "version": cv.Version}).Debug("fetching files")

	url := chartTarballURL(r, cv)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent())
	if len(r.AuthorizationHeader) > 0 {
		req.Header.Set("Authorization", r.AuthorizationHeader)
	}

	res, err := netClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	// We read the whole chart into memory, this should be okay since the chart
	// tarball needs to be small enough to fit into a GRPC call (Tiller
	// requirement)
	gzf, err := gzip.NewReader(res.Body)
	if err != nil {
		return err
	}
	defer gzf.Close()

	tarf := tar.NewReader(gzf)

	readmeFileName := name + "/README.md"
	valuesFileName := name + "/values.yaml"
	schemaFileName := name + "/values.schema.json"
	filenames := []string{valuesFileName, readmeFileName, schemaFileName}

	files, err := extractFilesFromTarball(filenames, tarf)
	if err != nil {
		return err
	}

	chartFiles := chartFiles{ID: chartFilesID, Repo: r, Digest: cv.Digest}
	if v, ok := files[readmeFileName]; ok {
		chartFiles.Readme = v
	} else {
		log.WithFields(log.Fields{"name": name, "version": cv.Version}).Info("README.md not found")
	}
	if v, ok := files[valuesFileName]; ok {
		chartFiles.Values = v
	} else {
		log.WithFields(log.Fields{"name": name, "version": cv.Version}).Info("values.yaml not found")
	}
	if v, ok := files[schemaFileName]; ok {
		chartFiles.Schema = v
	} else {
		log.WithFields(log.Fields{"name": name, "version": cv.Version}).Info("values.schema.json not found")
	}

	// inserts the chart files if not already indexed, or updates the existing
	// entry if digest has changed
	db.C(chartFilesCollection).UpsertId(chartFilesID, chartFiles)

	return nil
}