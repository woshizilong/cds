package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/ovh/cds/engine/api/application"
	"github.com/ovh/cds/engine/api/ascode"
	"github.com/ovh/cds/engine/api/cache"
	"github.com/ovh/cds/engine/api/event"
	"github.com/ovh/cds/engine/api/operation"
	"github.com/ovh/cds/engine/api/project"
	"github.com/ovh/cds/engine/api/workflow"
	"github.com/ovh/cds/engine/service"
	"github.com/ovh/cds/sdk"
	"github.com/ovh/cds/sdk/exportentities"
	v2 "github.com/ovh/cds/sdk/exportentities/v2"
	"github.com/ovh/cds/sdk/log"
)

func (api *API) getWorkflowAsCodeHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		vars := mux.Vars(r)
		uuid := vars["uuid"]

		var ope sdk.Operation
		k := cache.Key(operation.CacheOperationKey, uuid)
		b, err := api.Cache.Get(k, &ope)
		if err != nil {
			log.Error(ctx, "cannot get from cache %s: %v", k, err)
		}
		if !b {
			return sdk.WithStack(sdk.ErrNotFound)
		}
		return service.WriteJSON(w, ope, http.StatusOK)
	}
}

// postWorkflowAsCodeHandler update an ascode workflow, this will create a pull request to target repository.
func (api *API) postWorkflowAsCodeHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		vars := mux.Vars(r)
		key := vars["key"]
		workflowName := vars["permWorkflowName"]

		migrate := FormBool(r, "migrate")
		branch := FormString(r, "branch")
		message := FormString(r, "message")

		u := getAPIConsumer(ctx)
		p, err := project.Load(api.mustDB(), key,
			project.LoadOptions.WithApplicationWithDeploymentStrategies,
			project.LoadOptions.WithPipelines,
			project.LoadOptions.WithEnvironments,
			project.LoadOptions.WithIntegrations,
			project.LoadOptions.WithClearKeys,
		)
		if err != nil {
			return err
		}

		wfDB, err := workflow.Load(ctx, api.mustDB(), api.Cache, *p, workflowName, workflow.LoadOptions{
			DeepPipeline:          migrate,
			WithAsCodeUpdateEvent: migrate,
			WithTemplate:          true,
		})
		if err != nil {
			return err
		}

		var rootApp *sdk.Application
		if wfDB.WorkflowData.Node.Context != nil && wfDB.WorkflowData.Node.Context.ApplicationID != 0 {
			rootApp, err = application.LoadByIDWithClearVCSStrategyPassword(api.mustDB(), wfDB.WorkflowData.Node.Context.ApplicationID)
			if err != nil {
				return err
			}
		}
		if rootApp == nil {
			return sdk.NewErrorFrom(sdk.ErrWrongRequest, "cannot find the root application of the workflow")
		}

		if migrate {
			if rootApp.VCSServer == "" || rootApp.RepositoryFullname == "" {
				return sdk.NewErrorFrom(sdk.ErrRepoNotFound, "no vcs configuration set on the root application of the given workflow")
			}
			return api.migrateWorkflowAsCode(ctx, w, *p, wfDB, *rootApp, branch, message)
		}

		if wfDB.FromRepository == "" {
			return sdk.NewErrorFrom(sdk.ErrForbidden, "cannot update a workflow that is not ascode")
		}

		if wfDB.TemplateInstance != nil {
			return sdk.NewErrorFrom(sdk.ErrForbidden, "cannot update a workflow that was generated by a template")
		}

		var wf sdk.Workflow
		if err := service.UnmarshalBody(r, &wf); err != nil {
			return err
		}

		if err := workflow.RenameNode(ctx, api.mustDB(), &wf); err != nil {
			return err
		}
		if err := workflow.IsValid(ctx, api.Cache, api.mustDB(), &wf, *p, workflow.LoadOptions{DeepPipeline: true}); err != nil {
			return err
		}

		var data exportentities.WorkflowComponents
		data.Workflow, err = exportentities.NewWorkflow(ctx, wf, v2.WorkflowSkipIfOnlyOneRepoWebhook)
		if err != nil {
			return err
		}

		ope, err := operation.PushOperationUpdate(ctx, api.mustDB(), api.Cache, *p, data, rootApp.VCSServer, rootApp.RepositoryFullname, branch, message, rootApp.RepositoryStrategy, u)
		if err != nil {
			return err
		}

		sdk.GoRoutine(context.Background(), fmt.Sprintf("UpdateAsCodeResult-%s", ope.UUID), func(ctx context.Context) {
			ed := ascode.EntityData{
				Name:          wfDB.Name,
				ID:            wfDB.ID,
				Type:          ascode.WorkflowEvent,
				FromRepo:      wfDB.FromRepository,
				OperationUUID: ope.UUID,
			}
			asCodeEvent := ascode.UpdateAsCodeResult(ctx, api.mustDB(), api.Cache, *p, *rootApp, ed, u)
			if asCodeEvent != nil {
				event.PublishAsCodeEvent(ctx, p.Key, *asCodeEvent, u)
			}
		}, api.PanicDump())

		return service.WriteJSON(w, ope, http.StatusOK)
	}
}

func (api *API) migrateWorkflowAsCode(ctx context.Context, w http.ResponseWriter, proj sdk.Project, wf *sdk.Workflow, app sdk.Application, branch, message string) error {
	u := getAPIConsumer(ctx)

	if wf.FromRepository != "" || (wf.FromRepository == "" && len(wf.AsCodeEvent) > 0) {
		return sdk.WithStack(sdk.ErrWorkflowAlreadyAsCode)
	}

	// Check if there is a repository web hook
	found := false
	for _, h := range wf.WorkflowData.GetHooks() {
		if h.HookModelName == sdk.RepositoryWebHookModelName {
			found = true
			break
		}
	}
	if !found {
		h := sdk.NodeHook{
			Config:        sdk.RepositoryWebHookModel.DefaultConfig.Clone(),
			HookModelName: sdk.RepositoryWebHookModel.Name,
		}
		wf.WorkflowData.Node.Hooks = append(wf.WorkflowData.Node.Hooks, h)

		if err := workflow.Update(ctx, api.mustDB(), api.Cache, proj, wf, workflow.UpdateOptions{}); err != nil {
			return err
		}
	}

	data, err := workflow.Pull(ctx, api.mustDB(), api.Cache, proj, wf.Name, project.EncryptWithBuiltinKey, v2.WorkflowSkipIfOnlyOneRepoWebhook)
	if err != nil {
		return err
	}

	ope, err := operation.PushOperation(ctx, api.mustDB(), api.Cache, proj, data, app.VCSServer, app.RepositoryFullname, branch, message, app.RepositoryStrategy, u)
	if err != nil {
		return err
	}

	sdk.GoRoutine(context.Background(), fmt.Sprintf("MigrateWorkflowAsCodeResult-%s", ope.UUID), func(ctx context.Context) {
		ed := ascode.EntityData{
			FromRepo:      ope.URL,
			Type:          ascode.WorkflowEvent,
			ID:            wf.ID,
			Name:          wf.Name,
			OperationUUID: ope.UUID,
		}
		asCodeEvent := ascode.UpdateAsCodeResult(ctx, api.mustDB(), api.Cache, proj, app, ed, u)
		if asCodeEvent != nil {
			event.PublishAsCodeEvent(ctx, proj.Key, *asCodeEvent, u)
		}
	}, api.PanicDump())

	return service.WriteJSON(w, ope, http.StatusOK)
}
