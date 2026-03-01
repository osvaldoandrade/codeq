package controllers

import (
	"encoding/json"

	"github.com/gin-gonic/gin"
)

func jsonMarshal(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func getTenantID(c *gin.Context) string {
	v, ok := c.Get("tenantID")
	if !ok {
		return ""
	}
	tid, ok := v.(string)
	if !ok {
		return ""
	}
	return tid
}
