package gateway

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"strconv"
	"strings"
)

const maxDeepSeekFileSize int64 = 64 * 1024 * 1024

type deepSeekFilesProvider struct{}

func (deepSeekFilesProvider) ValidateUpload(contentType string, body []byte) error {
	return validateDeepSeekFilesRequest(contentType, body)
}

func (deepSeekFilesProvider) UpstreamURL(baseURL string) string {
	baseURL = strings.TrimRight(baseURL, "/")
	baseURL = strings.TrimSuffix(baseURL, "/v1")
	return baseURL + "/files"
}

func validateDeepSeekFilesRequest(contentType string, body []byte) error {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" {
		return fmt.Errorf("Files 请求必须使用 multipart/form-data")
	}
	boundary := params["boundary"]
	if boundary == "" {
		return fmt.Errorf("Files 请求缺少 multipart boundary")
	}

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	seen := make(map[string]bool)
	var fileSize int64
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("解析 Files multipart 请求失败: %w", err)
		}

		fieldName := part.FormName()
		if fieldName == "" {
			return fmt.Errorf("Files multipart part 缺少字段名")
		}
		if seen[fieldName] {
			return fmt.Errorf("Files 请求字段 %q 重复", fieldName)
		}
		seen[fieldName] = true

		switch fieldName {
		case "file":
			if part.FileName() == "" {
				return fmt.Errorf("Files 请求缺少文件名")
			}
			fileSize, err = countDeepSeekFileSize(part)
			if err != nil {
				return err
			}
		case "purpose", "expires_after[anchor]", "expires_after[seconds]":
			value, err := readFilesPartValue(part)
			if err != nil {
				return err
			}
			if err := validateDeepSeekFilesField(fieldName, value); err != nil {
				return err
			}
		default:
			return fmt.Errorf("Files 请求不支持字段 %q", fieldName)
		}
	}

	if !seen["file"] || fileSize == 0 {
		return fmt.Errorf("Files 请求必须包含非空 file 文件")
	}
	if !seen["purpose"] {
		return fmt.Errorf("Files 请求缺少 purpose=user_data")
	}
	if seen["expires_after[anchor]"] != seen["expires_after[seconds]"] {
		return fmt.Errorf("expires_after[anchor] 和 expires_after[seconds] 必须同时提供")
	}
	return nil
}

func validateDeepSeekFilesField(fieldName, value string) error {
	switch fieldName {
	case "purpose":
		if value != "user_data" {
			return fmt.Errorf("Files purpose 必须是 user_data")
		}
	case "expires_after[anchor]":
		if value != "created_at" {
			return fmt.Errorf("expires_after[anchor] 必须是 created_at")
		}
	case "expires_after[seconds]":
		seconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil || seconds < 3600 || seconds > 2592000 {
			return fmt.Errorf("expires_after[seconds] 必须在 3600 到 2592000 之间")
		}
	}
	return nil
}

func countDeepSeekFileSize(part *multipart.Part) (int64, error) {
	reader := io.LimitReader(part, maxDeepSeekFileSize+1)
	size, err := io.Copy(io.Discard, reader)
	if err != nil {
		return 0, fmt.Errorf("读取 Files 文件失败: %w", err)
	}
	if size > maxDeepSeekFileSize {
		return 0, fmt.Errorf("Files 文件超过 64 MiB")
	}
	return size, nil
}

func readFilesPartValue(part *multipart.Part) (string, error) {
	value, err := io.ReadAll(io.LimitReader(part, 1024))
	if err != nil {
		return "", fmt.Errorf("读取 Files 字段失败: %w", err)
	}
	if len(value) == 1024 {
		return "", fmt.Errorf("Files 字段值过长")
	}
	return string(value), nil
}
