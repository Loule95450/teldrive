package services

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/divyam234/teldrive/cache"
	"github.com/divyam234/teldrive/models"
	"github.com/divyam234/teldrive/schemas"
	"github.com/divyam234/teldrive/utils"

	"github.com/divyam234/teldrive/types"

	"github.com/gin-gonic/gin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/mitchellh/mapstructure"
	range_parser "github.com/quantumsheep/range-parser"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type FileService struct {
	Db        *gorm.DB
	ChannelID int64
}

func getAuthUserId(c *gin.Context) int {
	val, _ := c.Get("jwtUser")
	jwtUser := val.(*types.JWTClaims)
	userId, _ := strconv.Atoi(jwtUser.Subject)
	return userId
}

func (fs *FileService) CreateFile(c *gin.Context) (*schemas.FileOut, *types.AppError) {
	userId := getAuthUserId(c)
	var fileIn schemas.FileIn
	if err := c.ShouldBindJSON(&fileIn); err != nil {
		return nil, &types.AppError{Error: errors.New("invalid request payload"), Code: http.StatusBadRequest}
	}

	fileIn.Path = strings.TrimSpace(fileIn.Path)

	if fileIn.Path != "" {
		var parent models.File
		if err := fs.Db.Where("type = ? AND path = ?", "folder", fileIn.Path).First(&parent).Error; err != nil {
			return nil, &types.AppError{Error: errors.New("parent directory not found"), Code: http.StatusNotFound}
		}
		fileIn.ParentID = parent.ID
	}

	if fileIn.Type == "folder" {
		fileIn.MimeType = "drive/folder"
		var fullPath string
		if fileIn.Path == "/" {
			fullPath = "/" + fileIn.Name
		} else {
			fullPath = fileIn.Path + "/" + fileIn.Name
		}
		fileIn.Path = fullPath
		fileIn.Depth = utils.IntPointer(len(strings.Split(fileIn.Path, "/")) - 1)
	} else if fileIn.Type == "file" {
		fileIn.Path = ""
		fileIn.ChannelID = &fs.ChannelID
	}

	fileIn.UserID = userId
	fileIn.Starred = utils.BoolPointer(false)
	fileIn.Status = "active"

	fileDb := mapFileInToFile(fileIn)

	if err := fs.Db.Create(&fileDb).Error; err != nil {
		return nil, &types.AppError{Error: errors.New("failed to create a file"), Code: http.StatusBadRequest}

	}

	res := mapFileToFileOut(fileDb)

	return &res, nil
}

func (fs *FileService) UpdateFile(c *gin.Context) (*schemas.FileOut, *types.AppError) {

	fileID := c.Param("fileID")

	var fileUpdate schemas.FileIn

	var files []models.File

	if err := c.ShouldBindJSON(&fileUpdate); err != nil {
		return nil, &types.AppError{Error: errors.New("invalid request payload"), Code: http.StatusBadRequest}
	}

	if fileUpdate.Type == "folder" && fileUpdate.Name != "" {
		if err := fs.Db.Raw("select * from teldrive.update_folder(?, ?)", fileID, fileUpdate.Name).Scan(&files).Error; err != nil {
			return nil, &types.AppError{Error: errors.New("failed to update the file"), Code: http.StatusInternalServerError}
		}
	} else {
		fileDb := mapFileInToFile(fileUpdate)
		if err := fs.Db.Model(&files).Clauses(clause.Returning{}).Where("id = ?", fileID).Updates(fileDb).Error; err != nil {
			return nil, &types.AppError{Error: errors.New("failed to update the file"), Code: http.StatusInternalServerError}
		}
	}

	if len(files) == 0 {
		return nil, &types.AppError{Error: errors.New("file not updated"), Code: http.StatusNotFound}
	}

	file := mapFileToFileOut(files[0])

	return &file, nil

}

func (fs *FileService) GetFileByID(c *gin.Context) (*schemas.FileOutFull, error) {

	fileID := c.Param("fileID")

	var file []models.File

	fs.Db.Model(&models.File{}).Where("id = ?", fileID).Find(&file)

	if len(file) == 0 {
		return nil, errors.New("file not found")
	}

	return mapFileToFileOutFull(file[0]), nil
}

func (fs *FileService) ListFiles(c *gin.Context) (*schemas.FileResponse, *types.AppError) {

	userId := getAuthUserId(c)

	var pagingParams schemas.PaginationQuery
	pagingParams.PerPage = 200
	if err := c.ShouldBindQuery(&pagingParams); err != nil {
		return nil, &types.AppError{Error: errors.New(""), Code: http.StatusBadRequest}
	}

	var sortingParams schemas.SortingQuery
	sortingParams.Order = "asc"
	sortingParams.Sort = "name"
	if err := c.ShouldBindQuery(&sortingParams); err != nil {
		return nil, &types.AppError{Error: errors.New(""), Code: http.StatusBadRequest}
	}

	var fileQuery schemas.FileQuery
	fileQuery.Op = "list"
	fileQuery.Status = "active"
	fileQuery.UserId = userId
	if err := c.ShouldBindQuery(&fileQuery); err != nil {
		return nil, &types.AppError{Error: errors.New(""), Code: http.StatusBadRequest}
	}

	query := fs.Db.Model(&models.File{}).Limit(pagingParams.PerPage)

	if fileQuery.Op == "list" {
		filters := []string{}
		filters = setOrderFilter(&pagingParams, &sortingParams, filters)

		query = query.Order("type DESC").Order(getOrder(sortingParams)).
			Where(map[string]interface{}{"user_id": userId, "status": "active"}).
			Where("parent_id in (?)", fs.Db.Model(&models.File{}).Select("id").Where("path = ?", fileQuery.Path)).
			Where(strings.Join(filters, " AND "))

	} else if fileQuery.Op == "find" {
		filters := []string{}

		filterQuery := map[string]interface{}{}

		err := mapstructure.Decode(fileQuery, &filterQuery)

		if err != nil {
			return nil, &types.AppError{Error: err, Code: http.StatusBadRequest}
		}

		delete(filterQuery, "op")

		if filterQuery["updated_at"] == nil {
			delete(filterQuery, "updated_at")
		}

		filters = setOrderFilter(&pagingParams, &sortingParams, filters)

		query = query.Order("type DESC").Order(getOrder(sortingParams)).Where(filterQuery).
			Where(filters)

	} else if fileQuery.Op == "search" {
		filters := []string{
			fmt.Sprintf("teldrive.get_tsquery('%s') @@ teldrive.get_tsvector(name)", fileQuery.Search),
		}
		filters = setOrderFilter(&pagingParams, &sortingParams, filters)

		query = query.Order(getOrder(sortingParams)).Where(strings.Join(filters, " AND "))
	}

	var results []schemas.FileOut

	query.Find(&results)

	token := ""

	if len(results) == pagingParams.PerPage {
		lastItem := results[len(results)-1]
		token = utils.GetField(&lastItem, utils.CamelToPascalCase(sortingParams.Sort))
		token = base64.StdEncoding.EncodeToString([]byte(token))
	}

	res := &schemas.FileResponse{Results: results, NextPageToken: token}

	return res, nil
}

func (fs *FileService) MoveFiles(c *gin.Context) (*schemas.Message, *types.AppError) {

	var payload schemas.FileOperation

	if err := c.ShouldBindJSON(&payload); err != nil {
		return nil, &types.AppError{Error: errors.New("invalid request payload"), Code: http.StatusBadRequest}
	}

	var destination models.File
	if err := fs.Db.Model(&models.File{}).Select("id").Where("path = ?", payload.Destination).First(&destination).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, &types.AppError{Error: errors.New("destination not found"), Code: http.StatusNotFound}

	}

	if err := fs.Db.Model(&models.File{}).Where("id IN ?", payload.Files).UpdateColumn("parent_id", destination.ID).Error; err != nil {
		return nil, &types.AppError{Error: errors.New("move failed"), Code: http.StatusInternalServerError}
	}

	return &schemas.Message{Status: true, Message: "files moved"}, nil
}

func (fs *FileService) DeleteFiles(c *gin.Context) (*schemas.Message, *types.AppError) {

	var payload schemas.FileOperation

	if err := c.ShouldBindJSON(&payload); err != nil {
		return nil, &types.AppError{Error: errors.New("invalid request payload"), Code: http.StatusBadRequest}
	}

	if err := fs.Db.Exec("call teldrive.delete_files($1)", payload.Files).Error; err != nil {
		return nil, &types.AppError{Error: errors.New("failed to delete files"), Code: http.StatusInternalServerError}
	}

	return &schemas.Message{Status: true, Message: "files deleted"}, nil
}

func (fs *FileService) GetFileStream(c *gin.Context) {

	w := c.Writer
	r := c.Request
	config := utils.GetConfig()

	fileID := c.Param("fileID")

	var tgClient *utils.Client

	var err error
	if config.MultiClient {
		tgClient = utils.GetBotClient()
		tgClient.Workload++

	} else {
		val, _ := c.Get("jwtUser")
		jwtUser := val.(*types.JWTClaims)
		userId, _ := strconv.Atoi(jwtUser.Subject)
		tgClient, _, err = utils.GetAuthClient(jwtUser.TgSession, userId)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	res, err := cache.CachedFunction(fs.GetFileByID, fmt.Sprintf("files:%s", fileID))(c)

	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	file := res.(*schemas.FileOutFull)

	w.Header().Set("Accept-Ranges", "bytes")

	var start, end int64

	rangeHeader := r.Header.Get("Range")

	if rangeHeader == "" {
		start = 0
		end = file.Size - 1
		w.WriteHeader(http.StatusOK)
	} else {
		ranges, err := range_parser.Parse(file.Size, r.Header.Get("Range"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		start = ranges[0].Start
		end = ranges[0].End
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, file.Size))
		w.WriteHeader(http.StatusPartialContent)
	}

	contentLength := end - start + 1

	w.Header().Set("Content-Type", file.MimeType)

	w.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))

	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=\"%s\"", file.Name))

	parts, err := fs.getParts(c, tgClient.Tg, file)

	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	parts = rangedParts(parts, int64(start), int64(end))

	ir, iw := io.Pipe()

	go func() {
		defer iw.Close()
		for _, part := range parts {
			streamFilePart(c, tgClient.Tg, iw, &part, part.Start, part.End, 1024*1024)
		}
	}()

	if r.Method != "HEAD" {
		io.CopyN(w, ir, contentLength)

	}

	defer func() {
		if config.MultiClient {
			tgClient.Workload--
		}
	}()
}

func (fs *FileService) getParts(ctx context.Context, tgClient *telegram.Client, file *schemas.FileOutFull) ([]types.Part, error) {

	ids := []tg.InputMessageID{}

	for _, part := range *file.Parts {
		ids = append(ids, tg.InputMessageID{ID: int(part.ID)})
	}

	s := make([]tg.InputMessageClass, len(ids))

	for i := range ids {
		s[i] = &ids[i]
	}

	api := tgClient.API()

	res, err := cache.CachedFunction(utils.GetChannelById, fmt.Sprintf("channels:%d", fs.ChannelID))(ctx, api, fs.ChannelID)

	if err != nil {
		return nil, err
	}

	channel := res.(*tg.Channel)

	messageRequest := tg.ChannelsGetMessagesRequest{Channel: &tg.InputChannel{ChannelID: fs.ChannelID, AccessHash: channel.AccessHash},
		ID: s}

	res, err = cache.CachedFunction(api.ChannelsGetMessages, fmt.Sprintf("messages:%s", file.ID))(ctx, &messageRequest)

	if err != nil {
		return nil, err
	}

	messages := res.(*tg.MessagesChannelMessages)

	parts := []types.Part{}

	for _, message := range messages.Messages {
		item := message.(*tg.Message)
		media := item.Media.(*tg.MessageMediaDocument)
		document := media.Document.(*tg.Document)
		location := document.AsInputDocumentFileLocation()
		parts = append(parts, types.Part{Location: location, Start: 0, End: document.Size - 1, Size: document.Size})
	}
	return parts, nil
}

func mapFileToFileOut(file models.File) schemas.FileOut {
	return schemas.FileOut{
		ID:        file.ID,
		Name:      file.Name,
		Type:      file.Type,
		MimeType:  file.MimeType,
		Path:      file.Path,
		Size:      file.Size,
		Starred:   file.Starred,
		ParentID:  file.ParentID,
		UpdatedAt: file.UpdatedAt,
	}
}

func mapFileInToFile(file schemas.FileIn) models.File {
	return models.File{
		Name:      file.Name,
		Type:      file.Type,
		MimeType:  file.MimeType,
		Path:      file.Path,
		Size:      file.Size,
		Starred:   file.Starred,
		Depth:     file.Depth,
		UserID:    file.UserID,
		ParentID:  file.ParentID,
		Parts:     file.Parts,
		ChannelID: file.ChannelID,
		Status:    file.Status,
	}
}

func mapFileToFileOutFull(file models.File) *schemas.FileOutFull {
	return &schemas.FileOutFull{
		FileOut: mapFileToFileOut(file),
		Parts:   file.Parts, ChannelID: file.ChannelID,
	}
}

func setOrderFilter(pagingParams *schemas.PaginationQuery, sortingParams *schemas.SortingQuery, filters []string) []string {
	if pagingParams.NextPageToken != "" {
		sortColumn := sortingParams.Sort
		if sortColumn == "name" {
			sortColumn = "name collate numeric"
		} else {
			sortColumn = utils.CamelToSnake(sortingParams.Sort)
		}

		tokenValue, err := base64.StdEncoding.DecodeString(pagingParams.NextPageToken)
		if err == nil {
			if sortingParams.Order == "asc" {
				filters = append(filters, fmt.Sprintf("%s > '%s'", sortColumn, string(tokenValue)))
			} else {
				filters = append(filters, fmt.Sprintf("%s < '%s'", sortColumn, string(tokenValue)))
			}
		}
	}
	return filters
}

func getOrder(sortingParams schemas.SortingQuery) string {
	sortColumn := utils.CamelToSnake(sortingParams.Sort)
	if sortingParams.Sort == "name" {
		sortColumn = "name collate numeric"
	}

	return fmt.Sprintf("%s %s", sortColumn, strings.ToUpper(sortingParams.Order))
}

func chunk(ctx context.Context, tgClient *telegram.Client, part *types.Part, offset int64, limit int64) ([]byte, error) {

	req := &tg.UploadGetFileRequest{
		Offset:   offset,
		Limit:    int(limit),
		Location: part.Location,
	}

	r, err := tgClient.API().UploadGetFile(ctx, req)

	if err != nil {
		return nil, err
	}

	switch result := r.(type) {
	case *tg.UploadFile:
		return result.Bytes, nil
	default:
		return nil, fmt.Errorf("unexpected type %T", r)
	}
}

func streamFilePart(ctx context.Context, tgClient *telegram.Client, writer *io.PipeWriter, part *types.Part, start, end, chunkSize int64) error {

	offset := start - (start % chunkSize)
	firstPartCut := start - offset
	lastPartCut := (end % chunkSize) + 1

	partCount := int(math.Ceil(float64(end+1)/float64(chunkSize))) - int(math.Floor(float64(offset)/float64(chunkSize)))

	currentPart := 1

	for {
		r, _ := chunk(ctx, tgClient, part, offset, chunkSize)

		if len(r) == 0 {
			break
		} else if partCount == 1 {
			r = r[firstPartCut:lastPartCut]

		} else if currentPart == 1 {
			r = r[firstPartCut:]

		} else if currentPart == partCount {
			r = r[:lastPartCut]

		}

		writer.Write(r)

		currentPart++

		offset += chunkSize

		if currentPart > partCount {
			break
		}

	}

	return nil
}

func rangedParts(parts []types.Part, start, end int64) []types.Part {

	chunkSize := parts[0].Size

	startPartNumber := utils.Max(int64(math.Ceil(float64(start)/float64(chunkSize)))-1, 0)

	endPartNumber := int64(math.Ceil(float64(end) / float64(chunkSize)))

	partsToDownload := parts[startPartNumber:endPartNumber]
	partsToDownload[0].Start = start % chunkSize
	partsToDownload[len(partsToDownload)-1].End = end % chunkSize

	return partsToDownload
}