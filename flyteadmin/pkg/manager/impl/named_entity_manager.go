package impl

import (
	"context"
	"strconv"
	"strings"

	"github.com/lyft/flyteidl/gen/pb-go/flyteidl/core"

	"github.com/lyft/flytestdlib/contextutils"

	"github.com/lyft/flyteadmin/pkg/common"
	"github.com/lyft/flyteadmin/pkg/errors"
	"google.golang.org/grpc/codes"

	"github.com/lyft/flyteadmin/pkg/manager/impl/util"
	"github.com/lyft/flyteadmin/pkg/manager/impl/validation"
	"github.com/lyft/flyteadmin/pkg/manager/interfaces"
	"github.com/lyft/flyteadmin/pkg/repositories"
	repoInterfaces "github.com/lyft/flyteadmin/pkg/repositories/interfaces"
	"github.com/lyft/flyteadmin/pkg/repositories/transformers"
	runtimeInterfaces "github.com/lyft/flyteadmin/pkg/runtime/interfaces"
	"github.com/lyft/flyteidl/gen/pb-go/flyteidl/admin"
	"github.com/lyft/flytestdlib/logger"
	"github.com/lyft/flytestdlib/promutils"
)

const state = "state"

// System-generated workflows are meant to be hidden from the user by default. Therefore we always only show
// workflow-type named entities that have been user generated only.
var nonSystemGeneratedWorkflowsFilter, _ = common.NewSingleValueFilter(
	common.NamedEntityMetadata, common.NotEqual, state, admin.NamedEntityState_SYSTEM_GENERATED)
var defaultWorkflowsFilter, _ = common.NewWithDefaultValueFilter(
	strconv.Itoa(int(admin.NamedEntityState_NAMED_ENTITY_ACTIVE)), nonSystemGeneratedWorkflowsFilter)

type NamedEntityMetrics struct {
	Scope promutils.Scope
}

type NamedEntityManager struct {
	db      repositories.RepositoryInterface
	config  runtimeInterfaces.Configuration
	metrics NamedEntityMetrics
}

func (m *NamedEntityManager) UpdateNamedEntity(ctx context.Context, request admin.NamedEntityUpdateRequest) (
	*admin.NamedEntityUpdateResponse, error) {
	if err := validation.ValidateNamedEntityUpdateRequest(request); err != nil {
		logger.Debugf(ctx, "invalid request [%+v]: %v", request, err)
		return nil, err
	}
	ctx = contextutils.WithProjectDomain(ctx, request.Id.Project, request.Id.Domain)

	// Ensure entity exists before trying to update it
	_, err := util.GetNamedEntity(ctx, m.db, request.ResourceType, *request.Id)
	if err != nil {
		return nil, err
	}

	metadataModel := transformers.CreateNamedEntityModel(&request)
	err = m.db.NamedEntityRepo().Update(ctx, metadataModel)
	if err != nil {
		logger.Debugf(ctx, "Failed to update named_entity for [%+v] with err %v", request.Id, err)
		return nil, err
	}
	return &admin.NamedEntityUpdateResponse{}, nil
}

func (m *NamedEntityManager) GetNamedEntity(ctx context.Context, request admin.NamedEntityGetRequest) (
	*admin.NamedEntity, error) {
	if err := validation.ValidateNamedEntityGetRequest(request); err != nil {
		logger.Debugf(ctx, "invalid request [%+v]: %v", request, err)
		return nil, err
	}
	ctx = contextutils.WithProjectDomain(ctx, request.Id.Project, request.Id.Domain)
	return util.GetNamedEntity(ctx, m.db, request.ResourceType, *request.Id)
}

func (m *NamedEntityManager) getQueryFilters(referenceEntity core.ResourceType, requestFilters string) ([]common.InlineFilter, error) {
	filters := make([]common.InlineFilter, 0)
	if referenceEntity == core.ResourceType_WORKFLOW {
		filters = append(filters, defaultWorkflowsFilter)
	}

	if len(requestFilters) == 0 {
		return filters, nil
	}
	additionalFilters, err := util.ParseFilters(requestFilters, common.NamedEntity)
	if err != nil {
		return nil, err
	}
	for _, filter := range additionalFilters {
		if strings.Contains(filter.GetField(), state) {
			filterWithDefaultValue, err := common.NewWithDefaultValueFilter(
				strconv.Itoa(int(admin.NamedEntityState_NAMED_ENTITY_ACTIVE)), filter)
			if err != nil {
				return nil, err
			}
			filters = append(filters, filterWithDefaultValue)
		} else {
			filters = append(filters, filter)
		}
	}
	return filters, nil
}

func (m *NamedEntityManager) ListNamedEntities(ctx context.Context, request admin.NamedEntityListRequest) (
	*admin.NamedEntityList, error) {
	if err := validation.ValidateNamedEntityListRequest(request); err != nil {
		logger.Debugf(ctx, "invalid request [%+v]: %v", request, err)
		return nil, err
	}
	ctx = contextutils.WithProjectDomain(ctx, request.Project, request.Domain)

	// HACK: In order to filter by state (if requested) - we need to amend the filter to use COALESCE
	// e.g. eq(state, 1) becomes 'WHERE (COALESCE(state, 0) = '1')' since not every NamedEntity necessarily
	// has an entry, and therefore the default state value '0' (active), should be assumed.
	filters, err := m.getQueryFilters(request.ResourceType, request.Filters)
	if err != nil {
		return nil, err
	}
	var sortParameter common.SortParameter
	if request.SortBy != nil {
		sortParameter, err = common.NewSortParameter(*request.SortBy)
		if err != nil {
			return nil, err
		}
	}
	offset, err := validation.ValidateToken(request.Token)
	if err != nil {
		return nil, errors.NewFlyteAdminErrorf(codes.InvalidArgument,
			"invalid pagination token %s for ListNamedEntities", request.Token)
	}
	listInput := repoInterfaces.ListNamedEntityInput{
		ListResourceInput: repoInterfaces.ListResourceInput{
			Limit:         int(request.Limit),
			Offset:        offset,
			InlineFilters: filters,
			SortParameter: sortParameter,
		},
		Project:      request.Project,
		Domain:       request.Domain,
		ResourceType: request.ResourceType,
	}

	output, err := m.db.NamedEntityRepo().List(ctx, listInput)
	if err != nil {
		logger.Debugf(ctx, "Failed to list named entities of type: %s with project: %s, domain: %s. Returned error was: %v",
			request.ResourceType, request.Project, request.Domain, err)
		return nil, err
	}

	var token string
	if len(output.Entities) == int(request.Limit) {
		token = strconv.Itoa(offset + len(output.Entities))
	}
	entities := transformers.FromNamedEntityModels(output.Entities)
	return &admin.NamedEntityList{
		Entities: entities,
		Token:    token,
	}, nil

}

func NewNamedEntityManager(
	db repositories.RepositoryInterface,
	config runtimeInterfaces.Configuration,
	scope promutils.Scope) interfaces.NamedEntityInterface {

	metrics := NamedEntityMetrics{
		Scope: scope,
	}
	return &NamedEntityManager{
		db:      db,
		config:  config,
		metrics: metrics,
	}
}
