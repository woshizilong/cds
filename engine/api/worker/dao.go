package worker

import (
	"context"
	"strings"

	"github.com/go-gorp/gorp"

	"github.com/ovh/cds/engine/api/authentication"
	"github.com/ovh/cds/engine/api/database/gorpmapping"
	"github.com/ovh/cds/sdk"
)

func Insert(ctx context.Context, db gorp.SqlExecutor, w *sdk.Worker) error {
	dbData := &dbWorker{Worker: *w}
	if err := gorpmapping.InsertAndSign(ctx, db, dbData); err != nil {
		return err
	}
	*w = dbData.Worker
	return nil
}

// Delete remove worker from database, it also removes the associated access_token
func Delete(db gorp.SqlExecutor, id string) error {
	accessTokenID, err := db.SelectNullStr("SELECT auth_consumer_id FROM worker WHERE id = $1", id)
	if err != nil {
		return sdk.WithStack(err)
	}
	query := `DELETE FROM worker WHERE id = $1`
	if _, err := db.Exec(query, id); err != nil {
		return sdk.WithStack(err)
	}

	if accessTokenID.Valid {
		if err := authentication.DeleteConsumerByID(db, accessTokenID.String); err != nil {
			return err
		}
	}

	return nil
}

func LoadByConsumerID(ctx context.Context, db gorp.SqlExecutor, id string) (*sdk.Worker, error) {
	query := gorpmapping.NewQuery("SELECT * FROM worker WHERE auth_consumer_id = $1").Args(id)
	var w sdk.Worker
	found, err := gorpmapping.Get(ctx, db, query, &w)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, sdk.WithStack(sdk.ErrNotFound)
	}
	return &w, nil
}

func LoadByID(ctx context.Context, db gorp.SqlExecutor, id string) (*sdk.Worker, error) {
	query := gorpmapping.NewQuery("SELECT * FROM worker WHERE id = $1").Args(id)
	var w sdk.Worker
	found, err := gorpmapping.Get(ctx, db, query, &w)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, sdk.WithStack(sdk.ErrNotFound)
	}
	return &w, nil
}

func LoadAll(ctx context.Context, db gorp.SqlExecutor) ([]sdk.Worker, error) {
	var workers []sdk.Worker
	query := gorpmapping.NewQuery(`SELECT * FROM worker ORDER BY name ASC`)
	if err := gorpmapping.GetAll(ctx, db, query, &workers); err != nil {
		return nil, err
	}
	return workers, nil
}

func LoadByHatcheryID(ctx context.Context, db gorp.SqlExecutor, hatcheryID int64) ([]sdk.Worker, error) {
	var workers []sdk.Worker
	query := gorpmapping.NewQuery(`SELECT * FROM worker WHERE hatchery_id = $1 ORDER BY name ASC`).Args(hatcheryID)
	if err := gorpmapping.GetAll(ctx, db, query, &workers); err != nil {
		return nil, err
	}
	return workers, nil
}

func LoadDeadWorkers(ctx context.Context, db gorp.SqlExecutor, timeout float64, status []string) ([]sdk.Worker, error) {
	var workers []sdk.Worker
	query := gorpmapping.NewQuery(`SELECT *
				FROM worker
				WHERE status = ANY(string_to_array($1, ',')::text[])
				AND now() - last_beat > $2 * INTERVAL '1' SECOND
				ORDER BY last_beat ASC`).Args(strings.Join(status, ","), timeout)
	if err := gorpmapping.GetAll(ctx, db, query, &workers); err != nil {
		return nil, err
	}
	return workers, nil
}

// SetStatus sets job_run_id and status to building on given worker
func SetStatus(ctx context.Context, db gorp.SqlExecutor, workerID string, status string) error {
	w, err := LoadByID(ctx, db, workerID)
	if err != nil {
		return err
	}
	w.Status = status
	if status == sdk.StatusBuilding || status == sdk.StatusWaiting {
		w.JobRunID = nil
	}
	dbData := &dbWorker{Worker: *w}
	if err := gorpmapping.UpdateAndSign(ctx, db, dbData); err != nil {
		return err
	}
	return nil
}

// SetToBuilding sets job_run_id and status to building on given worker
func SetToBuilding(ctx context.Context, db gorp.SqlExecutor, workerID string, jobRunID int64, key []byte) error {
	w, err := LoadByID(ctx, db, workerID)
	if err != nil {
		return err
	}
	w.Status = sdk.StatusBuilding
	w.JobRunID = &jobRunID
	w.PrivateKey = key

	dbData := &dbWorker{Worker: *w}
	if err := gorpmapping.UpdateAndSign(ctx, db, dbData); err != nil {
		return err
	}
	return err
}

// LoadWorkerWithDecryptKey load worker with decrypted private key
func LoadWorkerWithDecryptKey(ctx context.Context, db gorp.SqlExecutor, workerID string) (sdk.Worker, error) {
	var work dbWorker
	query := gorpmapping.NewQuery(`SELECT * FROM worker WHERE id = $1`).Args(workerID)
	found, err := gorpmapping.Get(ctx, db, query, &work, gorpmapping.GetOptions.WithDecryption)
	if err != nil {
		return sdk.Worker{}, err
	}
	if !found {
		return sdk.Worker{}, sdk.ErrNotFound
	}
	isValid, err := gorpmapping.CheckSignature(work, work.Signature)
	if err != nil {
		return sdk.Worker{}, err
	}
	if !isValid {
		return sdk.Worker{}, sdk.ErrInvalidData
	}
	return work.Worker, err
}
