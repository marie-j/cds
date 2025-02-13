package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/go-gorp/gorp"

	"github.com/ovh/cds/engine/api/database/gorpmapping"
	"github.com/ovh/cds/engine/api/integration/artifact_manager"
	"github.com/ovh/cds/engine/cache"
	"github.com/ovh/cds/engine/gorpmapper"
	"github.com/ovh/cds/sdk"
)

var (
	KeyResult = cache.Key("run", "result")
)

func GetRunResultKey(runID int64, t sdk.WorkflowRunResultType, fileName string) string {
	return cache.Key(KeyResult, string(t), strconv.Itoa(int(runID)), fileName)
}

func CanUploadRunResult(ctx context.Context, db *gorp.DbMap, store cache.Store, wr sdk.WorkflowRun, runResultCheck sdk.WorkflowRunResultCheck) (bool, error) {
	// Check run
	if wr.ID != runResultCheck.RunID {
		return false, sdk.WrapError(sdk.ErrInvalidData, "unable to upload and artifact for this run")
	}
	if sdk.StatusIsTerminated(wr.Status) {
		return false, sdk.WrapError(sdk.ErrInvalidData, "unable to upload artifact on a terminated run")
	}

	// Check node run
	var nrs []sdk.WorkflowNodeRun
	for _, nodeRuns := range wr.WorkflowNodeRuns {
		if len(nodeRuns) < 1 {
			continue
		}
		// Get last noderun
		nodeRun := nodeRuns[0]
		if nodeRun.ID != runResultCheck.RunNodeID {
			continue
		}
		nrs = nodeRuns
		if sdk.StatusIsTerminated(nodeRun.Status) {
			return false, sdk.WrapError(sdk.ErrInvalidData, "unable to upload artifact on a terminated node run")
		}
	}
	if len(nrs) == 0 {
		return false, sdk.WrapError(sdk.ErrNotFound, "unable to find node run: %d", runResultCheck.RunNodeID)
	}

	// Check job data
	nodeRunJob, err := LoadNodeJobRun(ctx, db, store, runResultCheck.RunJobID)
	if err != nil {
		return false, err
	}
	if nodeRunJob.WorkflowNodeRunID != runResultCheck.RunNodeID {
		return false, sdk.WrapError(sdk.ErrInvalidData, "invalid node run %d", runResultCheck.RunNodeID)
	}
	if sdk.StatusIsTerminated(nodeRunJob.Status) {
		return false, sdk.WrapError(sdk.ErrInvalidData, "unable to upload artifact on a terminated job")
	}

	// We don't check duplicate filename duplicates for artifact manager
	if runResultCheck.ResultType == sdk.WorkflowRunResultTypeArtifactManager {
		return true, nil
	}

	// Check File Name
	runResults, err := LoadRunResultsByRunIDAndType(ctx, db, runResultCheck.RunID, runResultCheck.ResultType)
	if err != nil {
		return false, sdk.WrapError(err, "unable to load run results for run %d", runResultCheck.RunID)
	}
	for _, runResult := range runResults {
		var fileName string
		switch runResultCheck.ResultType {
		case sdk.WorkflowRunResultTypeArtifact:
			refArt, err := runResult.GetArtifact()
			if err != nil {
				return false, err
			}
			fileName = refArt.Name
		case sdk.WorkflowRunResultTypeCoverage:
			refCov, err := runResult.GetCoverage()
			if err != nil {
				return false, err
			}
			fileName = refCov.Name
		case sdk.WorkflowRunResultTypeStaticFile:
			refArt, err := runResult.GetStaticFile()
			if err != nil {
				return false, err
			}
			fileName = refArt.Name
		}

		if fileName != runResultCheck.Name {
			continue
		}

		// If we find a run result with same check, check subnumber
		var previousNodeRunUpload *sdk.WorkflowNodeRun
		for _, nr := range nrs {
			if nr.ID != runResult.WorkflowNodeRunID {
				continue
			}
			previousNodeRunUpload = &nr
			break
		}
		if previousNodeRunUpload == nil {
			return false, sdk.WrapError(sdk.ErrConflictData, "artifact %s has already been uploaded from another pipeline", runResultCheck.Name)
		}

		// Check Sub num
		if runResult.SubNum == nrs[0].SubNumber {
			return false, sdk.WrapError(sdk.ErrConflictData, "artifact %s has already been uploaded", runResultCheck.Name)
		}
		if runResult.SubNum > nrs[0].SubNumber {
			return false, sdk.WrapError(sdk.ErrConflictData, "artifact %s cannot be uploaded into a previous run", runResultCheck.Name)
		}
	}
	return true, nil
}

func AddResult(ctx context.Context, db *gorp.DbMap, store cache.Store, wr *sdk.WorkflowRun, runResult *sdk.WorkflowRunResult) error {
	var cacheKey string
	switch runResult.Type {
	case sdk.WorkflowRunResultTypeArtifact:
		var err error
		cacheKey, err = verifyAddResultArtifact(store, runResult)
		if err != nil {
			return err
		}
	case sdk.WorkflowRunResultTypeCoverage:
		var err error
		cacheKey, err = verifyAddResultCoverage(store, runResult)
		if err != nil {
			return err
		}
	case sdk.WorkflowRunResultTypeArtifactManager:
		var err error
		cacheKey, err = verifyAddResultArtifactManager(ctx, db, store, wr, runResult)
		if err != nil {
			return err
		}
	case sdk.WorkflowRunResultTypeStaticFile:
		var err error
		cacheKey, err = verifyAddResultStaticFile(store, runResult)
		if err != nil {
			return err
		}
	default:
		return sdk.WrapError(sdk.ErrInvalidData, "unknown result type %s", runResult.Type)
	}

	tx, err := db.Begin()
	if err != nil {
		return sdk.WithStack(err)
	}
	defer tx.Rollback() //nolint

	if err := insertResult(tx, runResult); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return sdk.WithStack(store.Delete(cacheKey))
}

// Check validity of the request + complete runResuklt with md5,size,type
func verifyAddResultArtifactManager(ctx context.Context, db gorp.SqlExecutor, store cache.Store, wr *sdk.WorkflowRun, newRunResult *sdk.WorkflowRunResult) (string, error) {
	artNewResult, err := newRunResult.GetArtifactManager()
	if err != nil {
		return "", err
	}

	// Check file in integration
	var artiInteg *sdk.WorkflowProjectIntegration
	for i := range wr.Workflow.Integrations {
		if !wr.Workflow.Integrations[i].ProjectIntegration.Model.ArtifactManager {
			continue
		}
		artiInteg = &wr.Workflow.Integrations[i]
	}
	if artiInteg == nil {
		return "", sdk.NewErrorFrom(sdk.ErrInvalidData, "you cannot add a artifact manager run result without an integration")
	}
	secrets, err := loadRunSecretWithDecryption(ctx, db, wr.ID, []string{fmt.Sprintf(SecretProjIntegrationContext, artiInteg.ProjectIntegrationID)})
	if err != nil {
		return "", err
	}
	var artifactManagerToken string
	for _, s := range secrets {
		if s.Name == fmt.Sprintf("cds.integration.artifact_manager.%s", sdk.ArtifactoryConfigToken) {
			artifactManagerToken = string(s.Value)
			break
		}
	}
	if artifactManagerToken == "" {
		return "", sdk.NewErrorFrom(sdk.ErrNotFound, "unable to find artifact manager token")
	}
	artifactClient, err := artifact_manager.NewClient(artiInteg.ProjectIntegration.Config[sdk.ArtifactoryConfigPlatform].Value, artiInteg.ProjectIntegration.Config[sdk.ArtifactoryConfigURL].Value, artifactManagerToken)
	if err != nil {
		return "", err
	}
	fileInfo, err := artifactClient.GetFileInfo(artNewResult.RepoName, artNewResult.Path)
	if err != nil {
		return "", err
	}
	artNewResult.Size = fileInfo.Size
	artNewResult.MD5 = fileInfo.Md5
	artNewResult.RepoType = fileInfo.Type
	if artNewResult.FileType == "" {
		artNewResult.FileType = artNewResult.RepoType
	}

	if err := artNewResult.IsValid(); err != nil {
		return "", err
	}
	dataUpdated, _ := json.Marshal(artNewResult)
	newRunResult.DataRaw = dataUpdated

	// Check existing run-result duplicates
	var nrs []sdk.WorkflowNodeRun
	for _, nodeRuns := range wr.WorkflowNodeRuns {
		if len(nodeRuns) < 1 {
			continue
		}
		// Get last noderun
		nodeRun := nodeRuns[0]
		if nodeRun.ID != newRunResult.WorkflowNodeRunID {
			continue
		}
		nrs = nodeRuns
	}
	runResults, err := LoadRunResultsByRunIDAndType(ctx, db, wr.ID, newRunResult.Type)
	if err != nil {
		return "", sdk.WrapError(err, "unable to load run results for run %d", wr.ID)
	}
	for _, runResult := range runResults {
		artRunResult, _ := runResult.GetArtifactManager()
		// if name is different: no problem
		if artRunResult.Name != artNewResult.Name {
			continue
		}
		// if name is the same but type is different: no problem
		if artRunResult.RepoType != artNewResult.RepoType {
			continue
		}
		// It can also be a new run
		var previousNodeRunUpload *sdk.WorkflowNodeRun
		for _, nr := range nrs {
			if nr.ID != runResult.WorkflowNodeRunID {
				continue
			}
			previousNodeRunUpload = &nr
			break
		}
		if previousNodeRunUpload == nil {
			return "", sdk.NewErrorFrom(sdk.ErrConflictData, "run-result %s has already been created from another pipeline", artNewResult.Name)
		}
		// Check Sub num
		if runResult.SubNum == nrs[0].SubNumber {
			return "", sdk.NewErrorFrom(sdk.ErrConflictData, "run-result %s has already been created", artNewResult.Name)
		}
		if runResult.SubNum > nrs[0].SubNumber {
			return "", sdk.NewErrorFrom(sdk.ErrConflictData, "run-result %s cannot be created into a previous run", artNewResult.Name)
		}
	}

	cacheKey := GetRunResultKey(newRunResult.WorkflowRunID, newRunResult.Type, artNewResult.Name)
	b, err := store.Exist(cacheKey)
	if err != nil {
		return cacheKey, err
	}
	if !b {
		return cacheKey, sdk.WrapError(sdk.ErrForbidden, "unable to upload an unchecked artifact manager file")
	}
	return cacheKey, nil
}

func verifyAddResultCoverage(store cache.Store, runResult *sdk.WorkflowRunResult) (string, error) {
	coverageRunResult, err := runResult.GetCoverage()
	if err != nil {
		return "", err
	}
	if err := coverageRunResult.IsValid(); err != nil {
		return "", err
	}

	cacheKey := GetRunResultKey(runResult.WorkflowRunID, runResult.Type, coverageRunResult.Name)
	b, err := store.Exist(cacheKey)
	if err != nil {
		return cacheKey, err
	}
	if !b {
		return cacheKey, sdk.WrapError(sdk.ErrForbidden, "unable to upload an unchecked coverage")
	}
	return cacheKey, nil
}

func verifyAddResultArtifact(store cache.Store, runResult *sdk.WorkflowRunResult) (string, error) {
	artifactRunResult, err := runResult.GetArtifact()
	if err != nil {
		return "", err
	}
	if err := artifactRunResult.IsValid(); err != nil {
		return "", err
	}

	cacheKey := GetRunResultKey(runResult.WorkflowRunID, runResult.Type, artifactRunResult.Name)
	b, err := store.Exist(cacheKey)
	if err != nil {
		return cacheKey, err
	}
	if !b {
		return cacheKey, sdk.WrapError(sdk.ErrForbidden, "unable to upload an unchecked artifact")
	}
	return cacheKey, nil
}

func verifyAddResultStaticFile(store cache.Store, runResult *sdk.WorkflowRunResult) (string, error) {
	staticFileRunResult, err := runResult.GetStaticFile()
	if err != nil {
		return "", err
	}
	if err := staticFileRunResult.IsValid(); err != nil {
		return "", err
	}

	cacheKey := GetRunResultKey(runResult.WorkflowRunID, runResult.Type, staticFileRunResult.Name)
	b, err := store.Exist(cacheKey)
	if err != nil {
		return cacheKey, err
	}
	if !b {
		return cacheKey, sdk.WrapError(sdk.ErrForbidden, "unable to upload an unchecked static-file")
	}
	return cacheKey, nil
}

func insertResult(tx gorpmapper.SqlExecutorWithTx, runResult *sdk.WorkflowRunResult) error {
	runResult.ID = sdk.UUID()
	runResult.Created = time.Now()
	dbRunResult := dbRunResult(*runResult)
	if err := gorpmapping.Insert(tx, &dbRunResult); err != nil {
		return sdk.WithStack(err)
	}
	return nil
}

func getAll(ctx context.Context, db gorp.SqlExecutor, query gorpmapping.Query) (sdk.WorkflowRunResults, error) {
	var dbResults []dbRunResult
	if err := gorpmapping.GetAll(ctx, db, query, &dbResults); err != nil {
		return nil, err
	}
	results := make(sdk.WorkflowRunResults, 0, len(dbResults))
	for _, r := range dbResults {
		results = append(results, sdk.WorkflowRunResult(r))
	}
	return results, nil
}

func LoadRunResultsByRunID(ctx context.Context, db gorp.SqlExecutor, runID int64) (sdk.WorkflowRunResults, error) {
	query := gorpmapping.NewQuery("SELECT * FROM workflow_run_result WHERE workflow_run_id = $1 ORDER BY sub_num DESC").Args(runID)
	return getAll(ctx, db, query)
}

func LoadRunResultsByRunIDUnique(ctx context.Context, db gorp.SqlExecutor, runID int64) (sdk.WorkflowRunResults, error) {
	query := gorpmapping.NewQuery("SELECT * FROM workflow_run_result WHERE workflow_run_id = $1 ORDER BY sub_num DESC").Args(runID)
	rs, err := getAll(ctx, db, query)
	if err != nil {
		return nil, err
	}
	return rs.Unique()
}

func LoadRunResultsByNodeRunID(ctx context.Context, db gorp.SqlExecutor, nodeRunID int64) (sdk.WorkflowRunResults, error) {
	query := gorpmapping.NewQuery("SELECT * FROM workflow_run_result WHERE workflow_node_run_id = $1").Args(nodeRunID)
	return getAll(ctx, db, query)
}

func LoadRunResultsByRunIDAndType(ctx context.Context, db gorp.SqlExecutor, runID int64, t sdk.WorkflowRunResultType) (sdk.WorkflowRunResults, error) {
	query := gorpmapping.NewQuery("SELECT * FROM workflow_run_result WHERE workflow_run_id = $1 AND type = $2").Args(runID, t)
	return getAll(ctx, db, query)
}
