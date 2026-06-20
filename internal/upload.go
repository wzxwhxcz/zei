package internal

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/google/uuid"
)

// ErrRequestFailed 统一的请求失败错误
var ErrRequestFailed = errors.New("请求失败")

// MediaType 媒体类型
type MediaType string

const (
	MediaTypeImage MediaType = "image"
	MediaTypeVideo MediaType = "video"
)

// FileUploadResponse z.ai 文件上传响应
type FileUploadResponse struct {
	ID        string                 `json:"id"`
	UserID    string                 `json:"user_id"`
	Hash      *string                `json:"hash"`
	Filename  string                 `json:"filename"`
	Data      map[string]interface{} `json:"data"`
	Meta      FileMeta               `json:"meta"`
	CreatedAt int64                  `json:"created_at"`
	UpdatedAt int64                  `json:"updated_at"`
}

// FileMeta 文件元数据
type FileMeta struct {
	Name        string                 `json:"name"`
	ContentType string                 `json:"content_type"`
	Size        int64                  `json:"size"`
	Data        map[string]interface{} `json:"data"`
	OssEndpoint string                 `json:"oss_endpoint"`
	CdnURL      string                 `json:"cdn_url"`
}

// UpstreamFile 上游请求的文件格式
type UpstreamFile struct {
	Type       string             `json:"type"`
	File       FileUploadResponse `json:"file"`
	ID         string             `json:"id"`
	URL        string             `json:"url"`
	Name       string             `json:"name"`
	Status     string             `json:"status"`
	Size       int64              `json:"size"`
	Error      string             `json:"error"`
	ItemID     string             `json:"itemId"`
	Media      string             `json:"media"`
	UploadedAt int64              `json:"uploadedAt,omitempty"` // 真实请求里文本附件带此字段（毫秒时间戳），媒体附件不带
}

// mimeExtMap MIME 类型到扩展名映射
var mimeExtMap = map[string]string{
	// 图片
	"image/png":     ".png",
	"image/jpeg":    ".jpg",
	"image/jpg":     ".jpg",
	"image/gif":     ".gif",
	"image/webp":    ".webp",
	"image/bmp":     ".bmp",
	"image/svg+xml": ".svg",
	// 视频
	"video/mp4":        ".mp4",
	"video/webm":       ".webm",
	"video/quicktime":  ".mov",
	"video/x-msvideo":  ".avi",
	"video/mpeg":       ".mpeg",
	"video/x-matroska": ".mkv",
}

// detectMediaType 根据 MIME 类型判断媒体类型
func detectMediaType(contentType string) MediaType {
	if strings.HasPrefix(contentType, "video/") {
		return MediaTypeVideo
	}
	return MediaTypeImage
}

// getExtFromMime 根据 MIME 类型获取文件扩展名
func getExtFromMime(contentType string, mediaType MediaType) string {
	// 精确匹配
	if ext, ok := mimeExtMap[contentType]; ok {
		return ext
	}
	// 模糊匹配
	for mime, ext := range mimeExtMap {
		if strings.Contains(contentType, strings.TrimPrefix(mime, "image/")) ||
			strings.Contains(contentType, strings.TrimPrefix(mime, "video/")) {
			return ext
		}
	}
	// 默认
	if mediaType == MediaTypeVideo {
		return ".mp4"
	}
	return ".png"
}

func parseBase64Data(dataURL string) (data []byte, contentType string, err error) {
	parts := strings.SplitN(dataURL, ",", 2)
	if len(parts) != 2 {
		return nil, "", ErrRequestFailed
	}
	header := parts[0]
	if idx := strings.Index(header, ":"); idx != -1 {
		mimeAndEncoding := header[idx+1:]
		if semiIdx := strings.Index(mimeAndEncoding, ";"); semiIdx != -1 {
			contentType = mimeAndEncoding[:semiIdx]
		}
	}

	// 解码 base64
	data, err = base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		LogError("base64 decode error: %v", err)
		return nil, "", ErrRequestFailed
	}
	return data, contentType, nil
}

func isValidMediaMagicBytes(data []byte) bool {
	if len(data) < 12 {
		return false
	}
	// PNG: 89 50 4E 47
	if data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4E && data[3] == 0x47 {
		return true
	}
	// JPEG: FF D8 FF
	if data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
		return true
	}
	// GIF: 47 49 46 38
	if data[0] == 0x47 && data[1] == 0x49 && data[2] == 0x46 && data[3] == 0x38 {
		return true
	}
	// WebP: 52 49 46 46 ... 57 45 42 50
	if data[0] == 0x52 && data[1] == 0x49 && data[2] == 0x46 && data[3] == 0x46 &&
		len(data) > 11 && data[8] == 0x57 && data[9] == 0x45 && data[10] == 0x42 && data[11] == 0x50 {
		return true
	}
	// BMP: 42 4D
	if data[0] == 0x42 && data[1] == 0x4D {
		return true
	}
	// MP4/MOV: ftyp at offset 4
	if len(data) > 11 && data[4] == 0x66 && data[5] == 0x74 && data[6] == 0x79 && data[7] == 0x70 {
		return true
	}
	// WebM/MKV: 1A 45 DF A3
	if data[0] == 0x1A && data[1] == 0x45 && data[2] == 0xDF && data[3] == 0xA3 {
		return true
	}
	return false
}

func downloadFromURL(url string) (data []byte, contentType string, filename string, err error) {
	urlPreview := url
	if len(urlPreview) > 80 {
		urlPreview = urlPreview[:80] + "..."
	}
	LogDebug("[Download] Starting: %s", urlPreview)

	client, err := TLSHTTPClient(60 * time.Second)
	if err != nil {
		LogError("[Download] tls client: %v", err)
		return nil, "", "", ErrRequestFailed
	}

	req, err := fhttp.NewRequest("GET", url, nil)
	if err != nil {
		LogError("[Download] create request error: %v", err)
		return nil, "", "", ErrRequestFailed
	}
	ApplyBrowserFingerprintHeaders(req.Header)
	req.Header.Set("Accept", "image/*, video/*, */*")
	if strings.Contains(url, "qq.com") {
		req.Header.Set("Referer", "https://qq.com/")
	}

	resp, err := client.Do(req)
	if err != nil {
		LogError("[Download] request error: %v", err)
		return nil, "", "", ErrRequestFailed
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		LogError("[Download] failed: status %d", resp.StatusCode)
		return nil, "", "", ErrRequestFailed
	}

	data, err = io.ReadAll(resp.Body)
	if err != nil {
		LogError("[Download] read body error: %v", err)
		return nil, "", "", ErrRequestFailed
	}

	contentType = resp.Header.Get("Content-Type")
	LogDebug("[Download] Success: size=%d, contentType=%s", len(data), contentType)

	// 使用 magic bytes 验证是否为有效媒体文件
	if !isValidMediaMagicBytes(data) {
		LogError("[Download] invalid media (magic bytes), contentType=%s, size=%d", contentType, len(data))
		return nil, "", "", ErrRequestFailed
	}

	filename = filepath.Base(url)
	// 去掉 URL 参数
	if idx := strings.Index(filename, "?"); idx != -1 {
		filename = filename[:idx]
	}
	// 检查文件名是否有效（必须包含扩展名）
	if !strings.Contains(filename, ".") || len(filename) < 3 {
		filename = ""
	}
	return data, contentType, filename, nil
}

// uploadToZAI 上传文件到 z.ai
func uploadToZAI(token string, data []byte, filename string, contentType string) (*FileUploadResponse, error) {
	LogDebug("[UploadToZAI] Preparing request: filename=%s, contentType=%s, dataSize=%d", filename, contentType, len(data))
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// 创建带正确Content-Type的form part
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, filename))
	if contentType != "" {
		h.Set("Content-Type", contentType)
	} else {
		h.Set("Content-Type", "application/octet-stream")
	}

	part, err := writer.CreatePart(h)
	if err != nil {
		LogError("create form part error: %v", err)
		return nil, ErrRequestFailed
	}

	if _, err := part.Write(data); err != nil {
		LogError("write file data error: %v", err)
		return nil, ErrRequestFailed
	}
	writer.Close()

	req, err := fhttp.NewRequest("POST", "https://chat.z.ai/api/v1/files/", &buf)
	if err != nil {
		LogError("create request error: %v", err)
		return nil, ErrRequestFailed
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "*/*")
	req.Header.Set("X-FE-Version", GetFeVersion())
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8,en-GB;q=0.7,en-US;q=0.6")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Cookie", "token="+token)
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Origin", "https://chat.z.ai")
	req.Header.Set("Referer", "https://chat.z.ai/")
	ApplyBrowserFingerprintHeaders(req.Header)
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Gpc", "1")
	req.Header.Set("DNT", "1")
	randomIP := generateRandomIP()
	req.Header.Set("X-Forwarded-For", randomIP)
	req.Header.Set("X-Real-IP", randomIP)

	client, err := TLSHTTPClient(120 * time.Second)
	if err != nil {
		LogError("tls client: %v", err)
		return nil, ErrRequestFailed
	}

	resp, err := client.Do(req)
	if err != nil {
		LogError("upload request error: %v", err)
		return nil, ErrRequestFailed
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		errBody := string(respBody)
		if len(errBody) > 200 {
			errBody = errBody[:200] + "..."
		}
		LogError("[Upload] failed: status %d, body: %s", resp.StatusCode, errBody)
		return nil, ErrRequestFailed
	}
	LogDebug("[UploadToZAI] Response body: %s", string(respBody))

	var uploadResp FileUploadResponse
	if err := json.Unmarshal(respBody, &uploadResp); err != nil {
		LogError("parse upload response error: %v", err)
		return nil, ErrRequestFailed
	}
	LogDebug("[UploadToZAI] Parsed response: id=%s, filename=%s, size=%d", uploadResp.ID, uploadResp.Filename, uploadResp.Meta.Size)
	return &uploadResp, nil
}

// isUnsupportedMediaURL 检查是否为不支持的媒体URL（如QQ临时链接只有appid没有文件id）
func isUnsupportedMediaURL(url string) bool {
	// QQ多媒体链接：如果只有appid参数没有实际文件id则跳过
	// 例如 https://multimedia.nt.qq.com.cn/download?appid=140 这种无效链接
	if strings.Contains(url, "multimedia.nt.qq.com.cn/download") {
		// 检查是否只有appid参数（没有其他参数如fileid等）
		if idx := strings.Index(url, "?"); idx != -1 {
			query := url[idx+1:]
			// 如果参数很短（只有appid=xxx），认为是无效链接
			if len(query) < 20 || !strings.Contains(query, "&") {
				return true
			}
		}
	}
	return false
}

// UploadMedia 通用媒体上传（支持图片和视频，支持 base64 和 URL）
func UploadMedia(token string, mediaURL string, mediaType MediaType) (*UpstreamFile, error) {
	var fileData []byte
	var filename string
	var contentType string

	// 记录上传开始
	urlPreview := mediaURL
	if len(urlPreview) > 100 {
		urlPreview = urlPreview[:100] + "..."
	}
	LogDebug("[Upload] Starting upload: type=%s, url=%s", mediaType, urlPreview)

	// 跳过不支持的URL
	if isUnsupportedMediaURL(mediaURL) {
		LogDebug("[Upload] Skipping unsupported media URL: %s", urlPreview)
		return nil, nil
	}

	if strings.HasPrefix(mediaURL, "data:") {
		// Base64 编码
		var err error
		fileData, contentType, err = parseBase64Data(mediaURL)
		if err != nil {
			LogDebug("[Upload] Base64 parse failed: %v", err)
			return nil, err
		}
		LogDebug("[Upload] Base64 parsed: contentType=%s, dataSize=%d bytes", contentType, len(fileData))
		// 根据 MIME 类型确定默认
		if contentType == "" {
			if mediaType == MediaTypeVideo {
				contentType = "video/mp4"
			} else {
				contentType = "image/png"
			}
		}
		ext := getExtFromMime(contentType, mediaType)
		filename = uuid.New().String()[:12] + ext
	} else {
		// 从 URL 下载
		var err error
		fileData, contentType, filename, err = downloadFromURL(mediaURL)
		if err != nil {
			LogDebug("[Upload] URL download failed: %v", err)
			return nil, err
		}
		LogDebug("[Upload] Downloaded from URL: filename=%s, contentType=%s, size=%d bytes", filename, contentType, len(fileData))
		// 检查文件名有效性
		if filename == "" || !strings.Contains(filename, ".") {
			if contentType == "" {
				if mediaType == MediaTypeVideo {
					contentType = "video/mp4"
				} else {
					contentType = "image/png"
				}
			}
			ext := getExtFromMime(contentType, mediaType)
			filename = fmt.Sprintf("pasted_%s_%d%s", mediaType, time.Now().UnixMilli(), ext)
		}
	}

	// 自动检测媒体类型
	if contentType != "" {
		detectedType := detectMediaType(contentType)
		if detectedType != mediaType {
			mediaType = detectedType
		}
	}

	// 上传到 z.ai
	LogDebug("[Upload] Uploading to z.ai: filename=%s, contentType=%s, size=%d bytes", filename, contentType, len(fileData))
	uploadResp, err := uploadToZAI(token, fileData, filename, contentType)
	if err != nil {
		LogDebug("[Upload] Upload to z.ai failed: %v", err)
		return nil, err
	}
	LogDebug("[Upload] Upload success: id=%s, cdnURL=%s", uploadResp.ID, uploadResp.Meta.CdnURL)

	return &UpstreamFile{
		Type:   string(mediaType),
		File:   *uploadResp,
		ID:     uploadResp.ID,
		URL:    "/api/v1/files/" + uploadResp.ID + "/content",
		Name:   uploadResp.Filename,
		Status: "uploaded",
		Size:   uploadResp.Meta.Size,
		Error:  "",
		ItemID: uuid.New().String(),
		Media:  string(mediaType),
	}, nil
}

// UploadImageFromURL 从 URL 或 base64 上传图片到 z.ai
func UploadImageFromURL(token string, imageURL string) (*UpstreamFile, error) {
	return UploadMedia(token, imageURL, MediaTypeImage)
}

// UploadVideoFromURL 从 URL 或 base64 上传视频到 z.ai
func UploadVideoFromURL(token string, videoURL string) (*UpstreamFile, error) {
	return UploadMedia(token, videoURL, MediaTypeVideo)
}

// UploadImages 批量上传图片
func UploadImages(token string, imageURLs []string) ([]*UpstreamFile, error) {
	LogDebug("[UploadImages] Starting batch upload: count=%d", len(imageURLs))
	var files []*UpstreamFile
	for i, url := range imageURLs {
		LogDebug("[UploadImages] Uploading image %d/%d", i+1, len(imageURLs))
		file, err := UploadImageFromURL(token, url)
		if err != nil {
			LogError("upload image failed: %s - %v", url[:min(50, len(url))], err)
			continue
		}
		if file == nil {
			LogDebug("[UploadImages] Image %d skipped (unsupported URL)", i+1)
			continue
		}
		LogDebug("[UploadImages] Image %d uploaded: id=%s", i+1, file.ID)
		files = append(files, file)
	}
	LogDebug("[UploadImages] Batch upload complete: success=%d/%d", len(files), len(imageURLs))
	return files, nil
}

// UploadVideos 批量上传视频
func UploadVideos(token string, videoURLs []string) ([]*UpstreamFile, error) {
	LogDebug("[UploadVideos] Starting batch upload: count=%d", len(videoURLs))
	var files []*UpstreamFile
	for i, url := range videoURLs {
		LogDebug("[UploadVideos] Uploading video %d/%d", i+1, len(videoURLs))
		file, err := UploadVideoFromURL(token, url)
		if err != nil {
			LogError("upload video failed: %s - %v", url[:min(50, len(url))], err)
			continue
		}
		if file == nil {
			LogDebug("[UploadVideos] Video %d skipped (unsupported URL)", i+1)
			continue
		}
		LogDebug("[UploadVideos] Video %d uploaded: id=%s", i+1, file.ID)
		files = append(files, file)
	}
	LogDebug("[UploadVideos] Batch upload complete: success=%d/%d", len(files), len(videoURLs))
	return files, nil
}

// UploadMediaFiles 批量上传媒体文件（图片+视频）
func UploadMediaFiles(token string, imageURLs, videoURLs []string) ([]*UpstreamFile, []*UpstreamFile, error) {
	images, _ := UploadImages(token, imageURLs)
	videos, _ := UploadVideos(token, videoURLs)
	return images, videos, nil
}

// buildHistoryTranscript 把多轮对话历史拼成一段纯文本，
// 作为上下文文件 GLM_CHAT_HISTORY.txt 的正文。z.ai 后端不读 messages 数组里的历史，
// 所以我们把历史序列化成 .txt 附到请求 files 数组里让模型读取（实测 z.ai 会读）。
// 思路参考 CJackHwang/ds2api，但用我们自己的命名与格式。
// lastUserIdx 之后的消息（即最新的 user 消息）不算历史，由请求主 messages 携带。
func buildHistoryTranscript(messages []Message, lastUserIdx int) string {
	if lastUserIdx <= 0 {
		return ""
	}
	var sb strings.Builder
	// 自己的标题：明确告诉模型这份文件是历史上下文
	sb.WriteString("# GLM_CHAT_HISTORY.txt\n")
	sb.WriteString("下面是本次对话之前的完整聊天记录，作为上下文供你参考。请结合用户最新消息一起理解。\n\n")
	for i := 0; i < lastUserIdx; i++ {
		m := messages[i]
		var role string
		switch m.Role {
		case "assistant":
			role = "AI"
		case "system":
			role = "系统"
		case "tool":
			role = "工具返回"
		default:
			role = "用户"
		}
		content, _ := m.ParseContent()
		if content == "" {
			continue
		}
		sb.WriteString(role + "：" + content + "\n\n")
	}
	return sb.String()
}

// UploadContextFile 把对话历史上传成 z.ai 文件，返回可附到请求 files 数组的结构。
// 失败（上传失败/正文为空/超阈值）时返回 (nil, nil)，调用方据此回退到「合并到最后一条消息」。
// mediaType 用 "file" 表示文本附件（媒体用 "image"/"video"）。
func UploadContextFile(token, transcript string) (*UpstreamFile, error) {
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		return nil, nil
	}
	if Cfg != nil && Cfg.ContextFileMaxBytes > 0 && len(transcript) > Cfg.ContextFileMaxBytes {
		LogDebug("[ContextFile] transcript %d bytes > limit %d, fallback to merge", len(transcript), Cfg.ContextFileMaxBytes)
		return nil, nil
	}
	filename := "GLM_CHAT_HISTORY.txt"
	LogDebug("[ContextFile] Uploading history transcript: %d bytes, filename=%s", len(transcript), filename)
	uploadResp, err := uploadToZAI(token, []byte(transcript), filename, "text/plain")
	if err != nil {
		LogDebug("[ContextFile] upload failed: %v, fallback to merge", err)
		return nil, nil // 回退，不把上传错误往上传
	}
	LogDebug("[ContextFile] upload success: id=%s", uploadResp.ID)
	// url 格式按真实 z.ai web 请求：/api/v1/files/<id>（不带 /content 后缀）。
	// uploadedAt 用毫秒时间戳，与真实请求一致。
	return &UpstreamFile{
		Type:       "file",
		File:       *uploadResp,
		ID:         uploadResp.ID,
		URL:        "/api/v1/files/" + uploadResp.ID,
		Name:       uploadResp.Filename,
		Status:     "uploaded",
		Size:       uploadResp.Meta.Size,
		ItemID:     uuid.New().String(),
		Media:      "file",
		UploadedAt: time.Now().UnixMilli(),
	}, nil
}
