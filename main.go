package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/altshiftab/utils_go/pkg/cloud/gws/gmail"
	"github.com/altshiftab/utils_go/pkg/cloud/gws/gmail/gmail_config"
	"github.com/altshiftab/utils_go/pkg/cloud/gws/gmail/types/watch_request"
	altshiftEnv "github.com/altshiftab/utils_go/pkg/env"
	altshiftErrors "github.com/altshiftab/utils_go/pkg/errors"
	"github.com/altshiftab/utils_go/pkg/errors/types/nil_error"
	"github.com/altshiftab/utils_go/pkg/http/types/fetch_config"
	altshiftHttpLogger "github.com/altshiftab/utils_go/pkg/log/http_logger"
	"github.com/altshiftab/utils_go/pkg/log/http_logger/http_logger_config"
	"github.com/altshiftab/utils_go/pkg/oauth2/types/config"
	"github.com/altshiftab/utils_go/pkg/oauth2/types/endpoint"
	"github.com/altshiftab/utils_go/pkg/oauth2/types/token"
)

const tokenUrl = "https://oauth2.googleapis.com/token"

func main() {
	logger := altshiftHttpLogger.New(http_logger_config.WithGcp(true))
	slog.SetDefault(logger.Logger)

	topicName := altshiftEnv.ReadEnvFatal("TOPIC_NAME")
	clientId := altshiftEnv.ReadEnvFatal("CLIENT_ID")
	clientSecret := altshiftEnv.ReadEnvFatal("CLIENT_SECRET")
	refreshToken := altshiftEnv.ReadEnvFatal("REFRESH_TOKEN")
	userId := altshiftEnv.ReadEnvFatal("USER_ID")

	var labelIds []string
	labelIdsString := altshiftEnv.GetEnvWithDefault("LABEL_IDS", "INBOX,SENT")
	if labelIdsString != "" {
		for _, id := range strings.Split(labelIdsString, ",") {
			if trimmed := strings.TrimSpace(id); trimmed != "" {
				labelIds = append(labelIds, trimmed)
			}
		}
	}

	ctx := context.Background()

	response, err := gmail.NewClient(
		gmail_config.WithFetchOptions(
			fetch_config.WithHttpClient(
				new(
					config.Config{
						ClientID:     clientId,
						ClientSecret: clientSecret,
						Endpoint:     endpoint.Endpoint{TokenURL: tokenUrl, AuthStyle: endpoint.AuthStyleInParams},
					},
				).Client(ctx, &token.Token{RefreshToken: refreshToken}),
			),
		),
	).Watch(
		ctx,
		userId,
		&watch_request.WatchRequest{
			TopicName:         topicName,
			LabelIds:          labelIds,
			LabelFilterAction: altshiftEnv.GetEnvWithDefault("LABEL_FILTER_ACTION", "include"),
		},
	)
	if err != nil {
		logger.FatalWithExitingMessage(
			"An error occurred when performing the watch.",
			fmt.Errorf("gmail client watch: %w", err),
		)
	}
	if response == nil {
		logger.FatalWithExitingMessage(
			"The response is nil.",
			altshiftErrors.NewWithTrace(nil_error.New("response")),
		)
	}

	logger.Info(
		"Watch registered.",
		slog.String("topicName", topicName),
		slog.String("userId", userId),
		slog.String("historyId", response.HistoryId),
		slog.String("expiration", response.Expiration),
	)
}
