package transferfiles

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/utils"
	coreUtils "github.com/jfrog/jfrog-cli-core/v2/artifactory/utils"
	commonTests "github.com/jfrog/jfrog-cli-core/v2/common/tests"
	coreConfig "github.com/jfrog/jfrog-cli-core/v2/utils/config"
	"github.com/jfrog/jfrog-client-go/artifactory"
	"github.com/jfrog/jfrog-client-go/artifactory/services"
	clientUtils "github.com/jfrog/jfrog-client-go/artifactory/services/utils"

	"github.com/stretchr/testify/assert"
)

func TestHandleStopInitAndClose(t *testing.T) {
	transferFilesCommand := NewTransferFilesCommand(nil, nil)

	finishStopping, _ := transferFilesCommand.handleStop(nil)
	finishStopping()
	assert.False(t, transferFilesCommand.ShouldStop())
}

const uuidTokenForTest = "af14706e-e0c1-4b7d-8791-6a18bd1fd339"

func TestValidateDataTransferPluginMinimumVersion(t *testing.T) {
	t.Run("valid version", func(t *testing.T) { testValidateDataTransferPluginMinimumVersion(t, "9.9.9", false) })
	t.Run("exact version", func(t *testing.T) {
		testValidateDataTransferPluginMinimumVersion(t, dataTransferPluginMinVersion, false)
	})
	t.Run("invalid version", func(t *testing.T) { testValidateDataTransferPluginMinimumVersion(t, "1.0.0", true) })
}

func testValidateDataTransferPluginMinimumVersion(t *testing.T, curVersion string, errorExpected bool) {
	var pluginVersion string
	testServer, serverDetails, _ := commonTests.CreateRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI == "/"+pluginsExecuteRestApi+"verifyCompatability" {
			content, err := json.Marshal(utils.VersionResponse{Version: pluginVersion})
			assert.NoError(t, err)
			_, err = w.Write(content)
			assert.NoError(t, err)
		}
	})
	defer testServer.Close()
	srcPluginManager := initSrcUserPluginServiceManager(t, serverDetails)

	pluginVersion = curVersion
	err := getAndValidateDataTransferPlugin(srcPluginManager)
	if errorExpected {
		assert.EqualError(t, err, getMinimalVersionErrorMsg(curVersion))
		return
	}
	assert.NoError(t, err)
}

func initSrcUserPluginServiceManager(t *testing.T, serverDetails *coreConfig.ServerDetails) *srcUserPluginService {
	srcPluginManager, err := createSrcRtUserPluginServiceManager(serverDetails)
	assert.NoError(t, err)
	return srcPluginManager
}

func TestVerifyConfigImportPluginNotInstalled(t *testing.T) {
	testServer, serverDetails, _ := commonTests.CreateRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI == "/"+pluginsExecuteRestApi+"dataTransferVersion" {
			w.WriteHeader(http.StatusNotFound)
			_, err := w.Write([]byte("Not found"))
			assert.NoError(t, err)
		}
	})
	defer testServer.Close()
	srcPluginManager := initSrcUserPluginServiceManager(t, serverDetails)

	_, err := srcPluginManager.version()
	assert.ErrorContains(t, err, "Response from Artifactory: 404 Not Found.")
}

func TestUploadChunkAndPollUploads(t *testing.T) {
	totalChunkStatusVisits := 0
	fileSample := FileRepresentation{
		Repo: "my-repo-local",
		Path: "rel-path",
		Name: "name-demo",
	}

	testServer, serverDetails, _ := initPollUploadsTestMockServer(t, &totalChunkStatusVisits, fileSample)
	defer testServer.Close()
	srcPluginManager := initSrcUserPluginServiceManager(t, serverDetails)

	uploadChunkAndPollThreeTimes(t, srcPluginManager, fileSample)

	// Assert that exactly 3 requests to chunk status were made - once when it is still in progress, once after done received and once to notify back to the source.
	assert.Equal(t, 3, totalChunkStatusVisits)
}

// Sends chunk to upload, polls on chunk three times - once when it is still in progress, once after done received and once to notify back to the source.
func uploadChunkAndPollThreeTimes(t *testing.T, srcPluginManager *srcUserPluginService, fileSample FileRepresentation) {
	curThreads = 8
	uploadTokensChan := make(chan string, 3)
	doneChan := make(chan bool, 1)
	var runWaitGroup sync.WaitGroup

	chunk := UploadChunk{}
	chunk.appendUploadCandidate(fileSample)
	stopped := uploadChunkWhenPossible(&phaseBase{srcUpService: srcPluginManager, Stoppable: &Stoppable{}}, chunk, uploadTokensChan, nil)
	assert.False(t, stopped)
	assert.Equal(t, 1, curProcessedUploadChunks)

	runWaitGroup.Add(1)
	go func() {
		defer runWaitGroup.Done()
		pollUploads(nil, srcPluginManager, uploadTokensChan, doneChan, nil, nil)
	}()
	// Let the whole process run for a few chunk status checks, then mark it as done.
	time.Sleep(5 * waitTimeBetweenChunkStatusSeconds * time.Second)
	doneChan <- true
	// Wait for the go routine to return.
	runWaitGroup.Wait()
}

func getUploadChunkMockResponse(t *testing.T, w http.ResponseWriter) {
	w.WriteHeader(http.StatusAccepted)
	resp := UploadChunkResponse{UuidTokenResponse: UuidTokenResponse{UuidToken: uuidTokenForTest}}
	writeMockResponse(t, w, resp)
}

func validateChunkStatusBody(t *testing.T, r *http.Request) {
	// Read body
	content, err := io.ReadAll(r.Body)
	assert.NoError(t, err)
	var actual UploadChunksStatusBody
	assert.NoError(t, json.Unmarshal(content, &actual))

	// Make sure all parameters as expected
	if len(actual.ChunksToDelete) == 0 {
		assert.Len(t, actual.AwaitingStatusChunks, 1)
		assert.Equal(t, uuidTokenForTest, actual.AwaitingStatusChunks[0])
	} else {
		assert.Len(t, actual.ChunksToDelete, 1)
		assert.Equal(t, uuidTokenForTest, actual.ChunksToDelete[0])
	}

}

func getChunkStatusMockInProgressResponse(t *testing.T, w http.ResponseWriter) {
	resp := UploadChunksStatusResponse{ChunksStatus: []ChunkStatus{
		{
			UuidTokenResponse: UuidTokenResponse{UuidToken: uuidTokenForTest},
			Status:            InProgress,
		},
	}}
	writeMockResponse(t, w, resp)
}

func getChunkStatusMockDoneResponse(t *testing.T, w http.ResponseWriter, file FileRepresentation) {
	resp := UploadChunksStatusResponse{ChunksStatus: []ChunkStatus{
		{
			UuidTokenResponse: UuidTokenResponse{UuidToken: uuidTokenForTest},
			Status:            Done,
			Files:             []FileUploadStatusResponse{{FileRepresentation: file, Status: Success, StatusCode: http.StatusOK}},
		},
	}}
	writeMockResponse(t, w, resp)
}

func getChunkStatusMockChunksDeletedResponse(t *testing.T, w http.ResponseWriter) {
	resp := UploadChunksStatusResponse{DeletedChunks: []string{uuidTokenForTest}}
	writeMockResponse(t, w, resp)
}

func writeMockResponse(t *testing.T, w http.ResponseWriter, resp interface{}) {
	content, err := json.Marshal(resp)
	assert.NoError(t, err)
	_, err = w.Write(content)
	assert.NoError(t, err)
}

func initPollUploadsTestMockServer(t *testing.T, totalChunkStatusVisits *int, file FileRepresentation) (*httptest.Server, *coreConfig.ServerDetails, artifactory.ArtifactoryServicesManager) {
	return commonTests.CreateRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI == "/"+pluginsExecuteRestApi+"uploadChunk" {
			getUploadChunkMockResponse(t, w)
		} else if r.RequestURI == "/"+pluginsExecuteRestApi+syncChunks {
			*totalChunkStatusVisits++
			validateChunkStatusBody(t, r)
			// If already visited chunk status, return status done this time.
			if *totalChunkStatusVisits == 1 {
				getChunkStatusMockInProgressResponse(t, w)
			} else if *totalChunkStatusVisits == 2 {
				getChunkStatusMockDoneResponse(t, w, file)
			} else {
				getChunkStatusMockChunksDeletedResponse(t, w)
			}
		}
	})
}

func TestGetSourceLocalRepositories(t *testing.T) {
	// Prepare mock server
	testServer, serverDetails, _ := commonTests.CreateRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI == "/api/repositories?type=local&packageType=" {
			// Reponse for GetWithFilter
			w.WriteHeader(http.StatusOK)
			response := &[]services.RepositoryDetails{{Key: "repo-1"}, {Key: "repo-2"}}
			bytes, err := json.Marshal(response)
			assert.NoError(t, err)
			_, err = w.Write(bytes)
			assert.NoError(t, err)
		}
	})
	defer testServer.Close()

	// Get and assert local repositories
	transferFilesCommand := NewTransferFilesCommand(serverDetails, nil)
	localRepos, err := transferFilesCommand.getSourceLocalRepositories()
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"repo-1", "repo-2"}, localRepos)
}

func TestGetAllTargetLocalRepositories(t *testing.T) {
	// Prepare mock server
	testServer, serverDetails, _ := commonTests.CreateRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI == "/api/storageinfo/calculate" {
			// Reponse for CalculateStorageInfo
			w.WriteHeader(http.StatusAccepted)
		} else if r.RequestURI == "/api/storageinfo" {
			// Reponse for GetStorageInfo
			w.WriteHeader(http.StatusOK)
			response := &clientUtils.StorageInfo{RepositoriesSummaryList: []clientUtils.RepositorySummary{{RepoKey: "repo-1"}, {RepoKey: "repo-2"}}}
			bytes, err := json.Marshal(response)
			assert.NoError(t, err)
			_, err = w.Write(bytes)
			assert.NoError(t, err)
		} else if r.RequestURI == "/api/repositories?type=local&packageType=" {
			// Reponse for GetWithFilter
			w.WriteHeader(http.StatusOK)
			response := &[]services.RepositoryDetails{{Key: "repo-1"}, {Key: "repo-2"}, {Key: "artifactory-build-info"}, {Key: "proj-build-info"}}
			bytes, err := json.Marshal(response)
			assert.NoError(t, err)
			_, err = w.Write(bytes)
			assert.NoError(t, err)
		}
	})
	defer testServer.Close()

	// Get and assert regular local and build info repositories of the target server
	transferFilesCommand := NewTransferFilesCommand(nil, serverDetails)
	storageInfoManager, err := coreUtils.NewStorageInfoManager(serverDetails)
	assert.NoError(t, err)
	transferFilesCommand.targetStorageInfoManager = storageInfoManager
	localRepos, err := transferFilesCommand.getAllTargetLocalRepositories()
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"repo-1", "repo-2", "artifactory-build-info", "proj-build-info"}, localRepos)
}

func TestInitStorageInfoManagers(t *testing.T) {
	sourceServerCalculated, targetServerCalculated := false, false
	// Prepare source mock server
	sourceTestServer, sourceServerDetails, _ := commonTests.CreateRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI == "/api/storageinfo/calculate" {
			w.WriteHeader(http.StatusAccepted)
			sourceServerCalculated = true
		}
	})
	defer sourceTestServer.Close()

	// Prepare target mock server
	targetTestServer, targetserverDetails, _ := commonTests.CreateRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI == "/api/storageinfo/calculate" {
			w.WriteHeader(http.StatusAccepted)
			targetServerCalculated = true
		}
	})
	defer targetTestServer.Close()

	// Init and assert storage info managers
	transferFilesCommand := NewTransferFilesCommand(sourceServerDetails, targetserverDetails)
	err := transferFilesCommand.initStorageInfoManagers()
	assert.NoError(t, err)
	assert.True(t, sourceServerCalculated)
	assert.True(t, targetServerCalculated)
}
