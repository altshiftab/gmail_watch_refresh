package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/altshiftab/utils_go/pkg/cloud/gws/gmail"
	"github.com/altshiftab/utils_go/pkg/cloud/gws/gmail/gmail_config"
	"github.com/altshiftab/utils_go/pkg/cloud/gws/gmail/types/watch_request"
	altshiftEnv "github.com/altshiftab/utils_go/pkg/env"
	altshiftErrors "github.com/altshiftab/utils_go/pkg/errors"
	"github.com/altshiftab/utils_go/pkg/errors/types/nil_error"
	"github.com/altshiftab/utils_go/pkg/http/types/fetch_config"
	"github.com/altshiftab/utils_go/pkg/http/types/http_context_extractor"
	"github.com/altshiftab/utils_go/pkg/http/types/http_context_extractor/http_context_extractor_config"
	altshiftErrorLogger "github.com/altshiftab/utils_go/pkg/log/error_logger"
	altshiftHttpLogger "github.com/altshiftab/utils_go/pkg/log/http_logger"
	"github.com/altshiftab/utils_go/pkg/log/http_logger/http_logger_config"
	"github.com/altshiftab/utils_go/pkg/oauth2/types/config"
	"github.com/altshiftab/utils_go/pkg/oauth2/types/endpoint"
	"github.com/altshiftab/utils_go/pkg/oauth2/types/token"
	"github.com/altshiftab/utils_go/pkg/schema"
)

// The job registers, or renews, the watch that has Gmail publish changes to a user's mailbox to a Pub/Sub topic. A
// watch lapses seven days after it was registered, and the job runs daily so that it never does.

const (
	defaultLabelIds          = "INBOX,SENT"
	defaultLabelFilterAction = "include"
)

var tokenUrl = &url.URL{Scheme: "https", Host: "oauth2.googleapis.com", Path: "/token"}

// settings is what the job is told through its environment.
type settings struct {
	TopicName         string
	ClientId          string
	ClientSecret      string
	RefreshToken      string
	UserId            string
	LabelIds          []string
	LabelFilterAction string
}

// parseLabelIds splits a comma-separated list of label ids, leaving out empty entries.
func parseLabelIds(value string) []string {
	var labelIds []string
	for id := range strings.SplitSeq(value, ",") {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			labelIds = append(labelIds, trimmed)
		}
	}
	return labelIds
}

func readSettings() (*settings, error) {
	var s settings

	type requiredVariable struct {
		name   string
		target *string
	}
	requiredVariables := []*requiredVariable{
		{name: "TOPIC_NAME", target: &s.TopicName},
		{name: "CLIENT_ID", target: &s.ClientId},
		{name: "CLIENT_SECRET", target: &s.ClientSecret},
		{name: "REFRESH_TOKEN", target: &s.RefreshToken},
		{name: "USER_ID", target: &s.UserId},
	}
	for _, variable := range requiredVariables {
		value, err := altshiftEnv.ReadEnv(variable.name)
		if err != nil {
			return nil, fmt.Errorf("read env: %w", err)
		}
		*variable.target = value
	}

	// An empty LABEL_IDS is the default rather than no labels, as GetEnvWithDefault reads it.
	s.LabelIds = parseLabelIds(altshiftEnv.GetEnvWithDefault("LABEL_IDS", defaultLabelIds))
	s.LabelFilterAction = altshiftEnv.GetEnvWithDefault("LABEL_FILTER_ACTION", defaultLabelFilterAction)

	return &s, nil
}

// newLogger makes the logger the job writes its entries for Cloud Logging through. The request to the token endpoint
// carries the client secret and the refresh token in its body, which the logger would otherwise write out whole
// with any error that comes to carry that request.
func newLogger(writer io.Writer, tokenUrl *url.URL) *altshiftErrorLogger.Logger {
	var maskedRequestBodyUrls []*schema.Url
	if tokenUrl != nil {
		maskedRequestBodyUrls = append(
			maskedRequestBodyUrls,
			&schema.Url{Domain: tokenUrl.Hostname(), Path: tokenUrl.Path},
		)
	}

	return altshiftHttpLogger.New(
		http_logger_config.WithGcp(true),
		http_logger_config.WithWriter(writer),
		http_logger_config.WithHttpContextExtractor(
			http_context_extractor.New(
				http_context_extractor_config.WithReplaceableMessages(http_logger_config.DefaultReplaceableMessages...),
				http_context_extractor_config.WithMaskedRequestBodyUrls(maskedRequestBodyUrls...),
			),
		),
	)
}

// run registers the watch, exchanging the refresh token for an access token at tokenUrl. A nil gmailBaseUrl is
// Gmail's own.
func run(ctx context.Context, logger *slog.Logger, settings *settings, tokenUrl *url.URL, gmailBaseUrl *url.URL) error {
	if logger == nil {
		return altshiftErrors.NewWithTrace(nil_error.New("logger"))
	}
	if settings == nil {
		return altshiftErrors.NewWithTrace(nil_error.New("settings"))
	}
	if tokenUrl == nil {
		return altshiftErrors.NewWithTrace(nil_error.New("token url"))
	}

	oauth2Config := &config.Config{
		ClientID:     settings.ClientId,
		ClientSecret: settings.ClientSecret,
		Endpoint:     endpoint.Endpoint{TokenURL: tokenUrl.String(), AuthStyle: endpoint.AuthStyleInParams},
	}
	httpClient := oauth2Config.Client(ctx, &token.Token{RefreshToken: settings.RefreshToken})

	client := gmail.NewClient(
		gmail_config.WithBaseUrl(gmailBaseUrl),
		gmail_config.WithFetchOptions(fetch_config.WithHttpClient(httpClient)),
	)

	request := &watch_request.WatchRequest{
		TopicName:         settings.TopicName,
		LabelIds:          settings.LabelIds,
		LabelFilterAction: settings.LabelFilterAction,
	}
	response, err := client.Watch(ctx, settings.UserId, request)
	if err != nil {
		return altshiftErrors.New(fmt.Errorf("gmail client watch: %w", err), settings.UserId, request)
	}
	if response == nil {
		return altshiftErrors.NewWithTrace(nil_error.New("watch response"))
	}

	logger.InfoContext(
		ctx,
		"Watch registered.",
		slog.String("topicName", settings.TopicName),
		slog.String("userId", settings.UserId),
		slog.String("historyId", response.HistoryId),
		slog.String("expiration", response.Expiration),
	)

	return nil
}

func main() {
	logger := newLogger(os.Stdout, tokenUrl)
	slog.SetDefault(logger.Logger)

	settings, err := readSettings()
	if err != nil {
		logger.FatalWithExitingMessage("An environment variable could not be obtained.", err)
	}

	if err := run(context.Background(), logger.Logger, settings, tokenUrl, nil); err != nil {
		logger.FatalWithExitingMessage("An error occurred when performing the watch.", err)
	}
}
