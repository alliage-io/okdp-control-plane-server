package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/okdp/okdp-gitops/treeedit"

	"github.com/okdp/okdp-control-plane-server/internal/apperr"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/service"
)

// deploySpy embeds the interface so only DeployService needs a body.
type deploySpy struct {
	service.ServiceService
	err error
}

func (s *deploySpy) DeployService(context.Context, string, models.ServiceRequest) (*models.ServiceInstance, error) {
	return nil, s.err
}

func deploy(t *testing.T, err error) (int, map[string]any) {
	t.Helper()
	engine := gin.New()
	engine.POST("/api/projects/:name/services", NewServiceHandler(&deploySpy{err: err}, nil).DeployService)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/projects/a/services", strings.NewReader(`{"service":"trino","instanceName":"b-c"}`))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(recorder, req)
	body := map[string]any{}
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	return recorder.Code, body
}

// Both are 409, but they are not the same mistake: the console must tell a
// duplicate instance from a release name another project already uses.
func TestDeployConflictsCarryDistinctCodes(t *testing.T) {
	code, body := deploy(t, &service.ReleaseNameTakenError{ErrReleaseTaken: &treeedit.ErrReleaseTaken{Release: "a-b-c", Project: "a-b", Instance: "c"}})
	if code != http.StatusConflict || body["code"] != ErrorCodeReleaseNameTaken ||
		body["error"] != "release name 'a-b-c' is already used by instance 'c' of project 'a-b'" {
		t.Fatalf("release collision: %d %v", code, body)
	}

	code, body = deploy(t, apperr.AlreadyExists("services", "b-c"))
	if code != http.StatusConflict || body["code"] != ErrorCodeInstanceExists ||
		body["error"] != "Instance 'b-c' already exists in project 'a'" {
		t.Fatalf("duplicate instance: %d %v", code, body)
	}
}

type renderedValuesStub struct {
	values []models.RenderedValues
	err    error
}

func (s renderedValuesStub) GetRenderedValues(context.Context, string, string) ([]models.RenderedValues, error) {
	return s.values, s.err
}

func getRenderedValues(t *testing.T, svc service.RenderedValuesService) (int, string) {
	t.Helper()
	h := NewServiceHandler(nil, nil)
	if svc != nil {
		h.SetRenderedValuesService(svc)
	}
	engine := gin.New()
	engine.GET("/api/projects/:name/services/:serviceName/values", h.GetRenderedValues)
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/projects/demo/services/sql/values", nil))
	return recorder.Code, recorder.Body.String()
}

func TestGetRenderedValues(t *testing.T) {
	code, body := getRenderedValues(t, renderedValuesStub{values: []models.RenderedValues{{Name: "demo-sql-trino-values", Chart: "trino", Values: "a: 1\n", ChangedLines: []int{1}}}})
	if code != http.StatusOK || !strings.Contains(body, `"changedLines":[1]`) || !strings.Contains(body, `"chart":"trino"`) {
		t.Fatalf("200: %d %s", code, body)
	}
	notFound := apperr.NotFound("services", "sql")
	if code, body := getRenderedValues(t, renderedValuesStub{err: notFound}); code != http.StatusNotFound || !strings.Contains(body, "not found in project 'demo'") {
		t.Fatalf("404: %d %s", code, body)
	}
	if code, _ := getRenderedValues(t, nil); code != http.StatusNotImplemented {
		t.Fatalf("no service: %d", code)
	}
}

// A compile refusal is a 400 the console can tell from the others: it shows
// the compiler's message, one problem per line.
func TestDeployThatDoesNotCompileCarriesItsCode(t *testing.T) {
	err := &treeedit.CompileError{Project: "a", Instance: "b-c", Err: errors.New("values don't meet the schema:\n- replicas: must be >= 1")}
	code, body := deploy(t, service.DoesNotCompile(err))
	if code != http.StatusBadRequest || body["code"] != ErrorCodeDoesNotCompile ||
		body["error"] != "instance a/b-c does not compile: values don't meet the schema:\n- replicas: must be >= 1" {
		t.Fatalf("compile refusal: %d %v", code, body)
	}
	// Its problems name the instance the compile refused.
	fields, _ := body["fields"].([]any)
	if len(fields) != 1 || fields[0].(map[string]any)["instance"] != "a/b-c" {
		t.Fatalf("fields: %v", body["fields"])
	}
}
