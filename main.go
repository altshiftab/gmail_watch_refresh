package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Motmedel/utils_go/pkg/cloud/gws/gmail"
	"github.com/Motmedel/utils_go/pkg/cloud/gws/gmail/gmail_config"
	"github.com/Motmedel/utils_go/pkg/cloud/gws/gmail/types/watch_request"
	motmedelEnv "github.com/Motmedel/utils_go/pkg/env"
	motmedelErrors "github.com/Motmedel/utils_go/pkg/errors"
	"github.com/Motmedel/utils_go/pkg/errors/types/nil_error"
	"github.com/Motmedel/utils_go/pkg/http/types/fetch_config"
	"github.com/Motmedel/utils_go/pkg/oauth2/types/config"
	"github.com/Motmedel/utils_go/pkg/oauth2/types/endpoint"
	"github.com/Motmedel/utils_go/pkg/oauth2/types/token"

	gcpUtilsLogger "github.com/altshiftab/gcp_utils/pkg/types/logger"
)

const tokenUrl = "https://oauth2.googleapis.com/token"

func main() {
	logger := gcpUtilsLogger.New()
	slog.SetDefault(logger.Logger)

	topicName := motmedelEnv.ReadEnvFatal("TOPIC_NAME")
	clientId := motmedelEnv.ReadEnvFatal("CLIENT_ID")
	clientSecret := motmedelEnv.ReadEnvFatal("CLIENT_SECRET")
	refreshToken := motmedelEnv.ReadEnvFatal("REFRESH_TOKEN")
	userId := motmedelEnv.ReadEnvFatal("USER_ID")

	var labelIds []string
	labelIdsString := motmedelEnv.GetEnvWithDefault("LABEL_IDS", "INBOX,SENT")
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
			LabelFilterAction: motmedelEnv.GetEnvWithDefault("LABEL_FILTER_ACTION", "include"),
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
			motmedelErrors.NewWithTrace(nil_error.New("response")),
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
