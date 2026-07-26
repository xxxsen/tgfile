package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/xxxsen/common/trace"
)

func TestPublicErrorUsesTrustedRequestContextTraceID(t *testing.T) {
	response := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(response)
	request := httptest.NewRequestWithContext(
		trace.WithTraceId(context.Background(), "trusted-trace-id"),
		http.MethodGet,
		"/_admin/api/v1/session",
		nil,
	)
	request.Header.Set("X-Request-ID", "untrusted-client-value")
	ginContext.Request = request

	(&Handler{}).writePublicError(
		ginContext,
		http.StatusBadRequest,
		"invalid_request",
		"请求参数无效",
		nil,
	)

	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Equal(t, "trusted-trace-id", response.Header().Get("X-Request-ID"))
	var envelope errorEnvelope
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
	require.Equal(t, "trusted-trace-id", envelope.Error.RequestID)
	require.NotContains(t, safeAdminErrorType(errors.New("sensitive path /private/data")), "/private/data")
}
