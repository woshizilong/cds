package workflow

import (
	"context"
	"fmt"

	"github.com/go-gorp/gorp"
	"github.com/lib/pq"

	"github.com/ovh/cds/engine/api/application"
	"github.com/ovh/cds/engine/api/ascode"
	"github.com/ovh/cds/engine/api/database/gorpmapping"
	"github.com/ovh/cds/engine/api/environment"
	"github.com/ovh/cds/engine/api/integration"
	"github.com/ovh/cds/engine/api/pipeline"
	"github.com/ovh/cds/engine/api/workflowtemplate"
	"github.com/ovh/cds/sdk"
	"github.com/ovh/cds/sdk/log"
)

type LoadAllWorkflowsOptionsFilters struct {
	ProjectKey   string
	WorkflowName string
	VCSServer    string
	Repository   string
	GroupIDs     []int64
}

type LoadAllWorkflowsOptionsLoaders struct {
	WithApplications       bool
	WithPipelines          bool
	WithEnvironments       bool
	WithIntegrations       bool
	WithIcon               bool
	WithAsCodeUpdateEvents bool
	WithTemplate           bool
}

type LoadAllWorkflowsOptions struct {
	Filters   LoadAllWorkflowsOptionsFilters
	Loaders   LoadAllWorkflowsOptionsLoaders
	Offset    int
	Limit     int
	Ascending bool
}

func (opt LoadAllWorkflowsOptions) Query() gorpmapping.Query {
	var queryString = `
    WITH 
    workflow_root_application_id AS (
        SELECT 
            id as "workflow_id", 
            project_id,
            name as "workflow_name",
            (workflow_data -> 'node' -> 'context' ->> 'application_id')::BIGINT as "root_application_id"
        FROM workflow
    ),
    project_permission AS (
        SELECT 
            project_id,
            ARRAY_AGG(group_id) as "groups"
        FROM project_group
        GROUP BY project_id
    ),
    selected_workflow AS (
        SELECT 
        project.id, 
            workflow_root_application_id.workflow_id, 
            project.projectkey, 
            workflow_name, 
            application.id, 
            application.name, 
            application.vcs_server, 
            application.repo_fullname, 
            project_permission.groups
            FROM workflow_root_application_id
        LEFT OUTER JOIN application ON application.id = root_application_id
        JOIN project ON project.id = workflow_root_application_id.project_id
        JOIN project_permission ON project_permission.project_id = project.id	
    )
    SELECT workflow.* , selected_workflow.projectkey as "project_key"
    FROM workflow 
    JOIN selected_workflow ON selected_workflow.workflow_id = workflow.id
    `

	var filters []string
	var args []interface{}
	if opt.Filters.ProjectKey != "" {
		filters = append(filters, "selected_workflow.projectkey = $%d")
		args = append(args, opt.Filters.ProjectKey)
	}
	if opt.Filters.WorkflowName != "" {
		filters = append(filters, "selected_workflow.workflow_name = $%d")
		args = append(args, opt.Filters.WorkflowName)
	}
	if opt.Filters.VCSServer != "" {
		filters = append(filters, "selected_workflow.vcs_server = $%d")
		args = append(args, opt.Filters.VCSServer)
	}
	if opt.Filters.Repository != "" {
		filters = append(filters, "selected_workflow.repo_fullname = $%d")
		args = append(args, opt.Filters.Repository)
	}
	if len(opt.Filters.GroupIDs) != 0 {
		filters = append(filters, "selected_workflow.groups && $%d")
		args = append(args, pq.Int64Array(opt.Filters.GroupIDs))
	}

	for i, f := range filters {
		if i == 0 {
			queryString += " WHERE "
		} else {
			queryString += " AND "
		}
		queryString += fmt.Sprintf(f, i+1)
	}

	var order = " ORDER BY selected_workflow.projectkey, selected_workflow.workflow_name "
	if opt.Ascending {
		order += "ASC"
	} else {
		order += "DESC"
	}
	queryString += order

	if opt.Offset != 0 {
		queryString += fmt.Sprintf(" OFFSET %d", opt.Offset)
	}

	if opt.Limit != 0 {
		queryString += fmt.Sprintf(" LIMIT %d", opt.Limit)
	}

	q := gorpmapping.NewQuery(queryString).Args(args...)

	log.Debug("workflow.LoadAllWorkflowsOptions.Query> %v", q)

	return q
}

func (opt LoadAllWorkflowsOptions) GetLoaders() []gorpmapping.GetOptionFunc {

	var loaders = []gorpmapping.GetOptionFunc{}

	if opt.Loaders.WithApplications {
		loaders = append(loaders, func(db gorp.SqlExecutor, i interface{}) error {
			ws := i.(*[]Workflow)
			return opt.withApplications(db, ws)
		})
	}

	if opt.Loaders.WithEnvironments {
		loaders = append(loaders, func(db gorp.SqlExecutor, i interface{}) error {
			ws := i.(*[]Workflow)
			return opt.withEnvironments(db, ws)
		})
	}

	if opt.Loaders.WithPipelines {
		loaders = append(loaders, func(db gorp.SqlExecutor, i interface{}) error {
			ws := i.(*[]Workflow)
			return opt.withPipelines(db, ws)
		})
	}

	if opt.Loaders.WithAsCodeUpdateEvents {
		loaders = append(loaders, func(db gorp.SqlExecutor, i interface{}) error {
			ws := i.(*[]Workflow)
			return opt.withAsCodeUpdateEvents(db, ws)
		})
	}

	if !opt.Loaders.WithIcon {
		loaders = append(loaders, func(db gorp.SqlExecutor, i interface{}) error {
			ws := i.(*[]Workflow)
			for j := range *ws {
				w := (*ws)[j]
				w.Icon = ""
			}
			return nil
		})
	}

	if opt.Loaders.WithIntegrations {
		loaders = append(loaders, func(db gorp.SqlExecutor, i interface{}) error {
			ws := i.(*[]Workflow)
			return opt.withIntegrations(db, ws)
		})
	}

	if opt.Loaders.WithTemplate {
		loaders = append(loaders, func(db gorp.SqlExecutor, i interface{}) error {
			ws := i.(*[]Workflow)
			return opt.withTemplates(db, ws)
		})
	}

	return loaders
}

func (opt LoadAllWorkflowsOptions) withEnvironments(db gorp.SqlExecutor, ws *[]Workflow) error {
	var mapIDs = map[int64]*sdk.Environment{}
	for _, w := range *ws {
		nodesArray := w.WorkflowData.Array()
		for _, n := range nodesArray {
			if n.Context != nil && n.Context.EnvironmentID != 0 {
				if _, ok := mapIDs[n.Context.EnvironmentID]; !ok {
					mapIDs[n.Context.EnvironmentID] = nil
				}
			}
		}
	}

	var ids = make([]int64, 0, len(mapIDs))
	for id := range mapIDs {
		ids = append(ids, id)
	}

	envs, err := environment.LoadAllByIDs(db, ids)
	if err != nil {
		return err
	}

	for id := range mapIDs {
		for i := range envs {
			if id == envs[i].ID {
				mapIDs[id] = &envs[i]
			}
		}
	}

	for x := range *ws {
		w := &(*ws)[x]
		w.InitMaps()
		nodesArray := w.WorkflowData.Array()
		for i := range nodesArray {
			n := nodesArray[i]
			if n.Context != nil && n.Context.EnvironmentID != 0 {
				if env, ok := mapIDs[n.Context.EnvironmentID]; ok {
					if env == nil {
						return sdk.WrapError(sdk.ErrNotFound, "unable to find environment %d", n.Context.EnvironmentID)
					}
					w.Environments[n.Context.EnvironmentID] = *env
				}
			}
		}
	}

	return nil
}

func (opt LoadAllWorkflowsOptions) withPipelines(db gorp.SqlExecutor, ws *[]Workflow) error {
	var mapIDs = map[int64]*sdk.Pipeline{}
	for _, w := range *ws {
		nodesArray := w.WorkflowData.Array()
		for _, n := range nodesArray {
			if n.Context != nil && n.Context.PipelineID != 0 {
				if _, ok := mapIDs[n.Context.PipelineID]; !ok {
					mapIDs[n.Context.PipelineID] = nil
				}
			}
		}
	}

	var ids = make([]int64, 0, len(mapIDs))
	for id := range mapIDs {
		ids = append(ids, id)
	}

	pips, err := pipeline.LoadAllByIDs(db, ids, false)
	if err != nil {
		return err
	}

	for id := range mapIDs {
		for i := range pips {
			if id == pips[i].ID {
				mapIDs[id] = &pips[i]
			}
		}
	}

	for x := range *ws {
		w := &(*ws)[x]
		w.InitMaps()
		nodesArray := w.WorkflowData.Array()
		for i := range nodesArray {
			n := nodesArray[i]
			if n.Context != nil && n.Context.PipelineID != 0 {
				if pip, ok := mapIDs[n.Context.PipelineID]; ok {
					if pip == nil {
						return sdk.WrapError(sdk.ErrNotFound, "unable to find pipeline %d", n.Context.PipelineID)
					}
					w.Pipelines[n.Context.PipelineID] = *pip
				}
			}
		}
	}

	return nil
}

func (opt LoadAllWorkflowsOptions) withTemplates(db gorp.SqlExecutor, ws *[]Workflow) error {
	var mapIDs = map[int64]struct{}{}
	for _, w := range *ws {
		mapIDs[w.ID] = struct{}{}
	}

	var ids = make([]int64, 0, len(mapIDs))
	for id := range mapIDs {
		ids = append(ids, id)
	}

	wtis, err := workflowtemplate.LoadInstanceByWorkflowIDs(context.Background(), db, ids, workflowtemplate.LoadInstanceOptions.WithTemplate)
	if err != nil {
		return err
	}

	for x := range *ws {
		w := &(*ws)[x]
		w.InitMaps()
		for _, wti := range wtis {
			if wti.WorkflowID != nil && w.ID == *wti.WorkflowID {
				w.TemplateInstance = &wti
				w.FromTemplate = fmt.Sprintf("%s@%d", wti.Template.Path(), wti.WorkflowTemplateVersion)
				w.TemplateUpToDate = wti.Template.Version == wti.WorkflowTemplateVersion
				break
			}
		}
	}

	return nil
}

func (opt LoadAllWorkflowsOptions) withIntegrations(db gorp.SqlExecutor, ws *[]Workflow) error {
	var mapIDs = map[int64]*sdk.ProjectIntegration{}
	for _, w := range *ws {
		nodesArray := w.WorkflowData.Array()
		for _, n := range nodesArray {
			if n.Context != nil && n.Context.ProjectIntegrationID != 0 {
				if _, ok := mapIDs[n.Context.ProjectIntegrationID]; !ok {
					mapIDs[n.Context.ProjectIntegrationID] = nil
				}
			}
		}
	}

	var ids = make([]int64, 0, len(mapIDs))
	for id := range mapIDs {
		ids = append(ids, id)
	}

	projectIntegrations, err := integration.LoadIntegrationsByIDs(db, ids)
	if err != nil {
		return err
	}

	for id := range mapIDs {
		for i := range projectIntegrations {
			if id == projectIntegrations[i].ID {
				mapIDs[id] = &projectIntegrations[i]
			}
		}
	}

	for x := range *ws {
		w := &(*ws)[x]
		w.InitMaps()
		nodesArray := w.WorkflowData.Array()
		for i := range nodesArray {
			n := nodesArray[i]
			if n.Context != nil && n.Context.ProjectIntegrationID != 0 {
				if integ, ok := mapIDs[n.Context.ProjectIntegrationID]; ok {
					if integ == nil {
						return sdk.WrapError(sdk.ErrNotFound, "unable to find integration %d", n.Context.ProjectIntegrationID)
					}
					w.ProjectIntegrations[n.Context.ProjectIntegrationID] = *integ
				}
			}
		}
	}

	return nil
}

func (opt LoadAllWorkflowsOptions) withAsCodeUpdateEvents(db gorp.SqlExecutor, ws *[]Workflow) error {
	var mapRepos = map[string][]sdk.AsCodeEvent{}
	for _, w := range *ws {
		if w.FromRepository != "" {
			mapRepos[w.FromRepository] = nil
		}
	}

	var repos = make([]string, 0, len(mapRepos))
	for repo := range mapRepos {
		repos = append(repos, repo)
	}

	asCodeEvents, err := ascode.LoadAsCodeEventByRepos(context.Background(), db, repos)
	if err != nil {
		return err
	}

	for repo := range mapRepos {
		for i := range asCodeEvents {
			if repo == asCodeEvents[i].FromRepo {
				mapRepos[repo] = append(mapRepos[repo], asCodeEvents[i])
			}
		}
	}

	for x := range *ws {
		w := &(*ws)[x]
		w.InitMaps()
		if w.FromRepository == "" {
			continue
		}
		if events, ok := mapRepos[w.FromRepository]; ok {
			w.AsCodeEvent = events
		}
	}

	return nil
}

func (opt LoadAllWorkflowsOptions) withApplications(db gorp.SqlExecutor, ws *[]Workflow) error {
	var mapIDs = map[int64]*sdk.Application{}
	for _, w := range *ws {
		nodesArray := w.WorkflowData.Array()
		for _, n := range nodesArray {
			if n.Context != nil && n.Context.ApplicationID != 0 {
				if _, ok := mapIDs[n.Context.ApplicationID]; !ok {
					mapIDs[n.Context.ApplicationID] = nil
				}
			}
		}
	}

	var ids = make([]int64, 0, len(mapIDs))
	for id := range mapIDs {
		ids = append(ids, id)
	}

	apps, err := application.LoadAllByIDs(db, ids)
	if err != nil {
		return err
	}

	for id := range mapIDs {
		for i := range apps {
			if id == apps[i].ID {
				mapIDs[id] = &apps[i]
			}
		}
	}

	for x := range *ws {
		w := &(*ws)[x]
		w.InitMaps()
		nodesArray := w.WorkflowData.Array()
		for i := range nodesArray {
			n := nodesArray[i]
			if n.Context != nil && n.Context.ApplicationID != 0 {
				if app, ok := mapIDs[n.Context.ApplicationID]; ok {
					if app == nil {
						return sdk.WrapError(sdk.ErrNotFound, "unable to find application %d", n.Context.ApplicationID)
					}
					w.Applications[n.Context.ApplicationID] = *app
				}
			}
		}
	}

	return nil
}

func LoadAllWorkflows(ctx context.Context, db gorp.SqlExecutor, opts LoadAllWorkflowsOptions) ([]sdk.Workflow, error) {
	var workflows []Workflow
	if err := gorpmapping.GetAll(ctx, db, opts.Query(), &workflows, opts.GetLoaders()...); err != nil {
		return nil, err
	}
	ws := make([]sdk.Workflow, 0, len(workflows))
	for i := range workflows {
		if err := workflows[i].PostGet(db); err != nil {
			return nil, err
		}
		w := workflows[i].Get()
		ws = append(ws, w)
	}
	return ws, nil
}
