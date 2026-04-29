package main

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/Hattorius/War-Era-Gateway/internal/database/models"
	"github.com/Hattorius/War-Era-Gateway/internal/scraper"
	gocache "github.com/patrickmn/go-cache"
	"gorm.io/gorm"
)

type trpcResponse struct {
	Result struct {
		Data struct {
			Items      []json.RawMessage `json:"items"`
			NextCursor string            `json:"nextCursor"`
		} `json:"data"`
	} `json:"result"`
}

func buildTRPCResponse(items []json.RawMessage, cursor string) (json.RawMessage, error) {
	resp := trpcResponse{}
	resp.Result.Data.Items = items
	resp.Result.Data.NextCursor = cursor
	return json.Marshal(resp)
}

func withFallback(
	ctx context.Context,
	s *scraper.Scraper,
	db *gorm.DB,
	method string,
	input json.RawMessage,
	dbQuery func() (json.RawMessage, error),
	upsertFn func(*gorm.DB, json.RawMessage) error,
) (json.RawMessage, error) {
	resp, err := dbQuery()
	if err != nil {
		return nil, err
	}

	var parsed trpcResponse
	err = json.Unmarshal(resp, &parsed)
	if err == nil && len(parsed.Result.Data.Items) > 0 && parsed.Result.Data.NextCursor != "" {
		return resp, nil
	}

	apiInput := withLimit100(input)
	raw, err := s.Request(ctx, method, apiInput)
	if err != nil {
		if len(parsed.Result.Data.Items) > 0 {
			return resp, nil
		}
		return nil, err
	}

	var apiResp trpcResponse
	err = json.Unmarshal(raw, &apiResp)
	if err == nil {
		for _, item := range apiResp.Result.Data.Items {
			if err := upsertFn(db, item); err != nil {
				slog.Error("Failed to upsert fallback item", "method", method, "error", err)
			}
		}
	}

	dbResp, err := dbQuery()
	if err != nil {
		return raw, nil
	}
	return dbResp, nil
}

func withLimit100(input json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(input, &m); err != nil {
		return input
	}
	m["limit"] = json.RawMessage("100")
	out, err := json.Marshal(m)
	if err != nil {
		return input
	}
	return out
}

func data_handler(
	ctx context.Context,
	c *gocache.Cache,
	s *scraper.Scraper,
	db *gorm.DB,
	method string,
	input json.RawMessage,
) (json.RawMessage, error) {
	switch method {
	case "event.getEventsPaginated":
		return withFallback(ctx, s, db, method, input, func() (json.RawMessage, error) {
			return handleEvents(db, input)
		}, models.UpsertEventFromJSON)
	case "workOffer.getWorkOffersPaginated":
		return withFallback(ctx, s, db, method, input, func() (json.RawMessage, error) {
			return handleWorkOffers(db, input)
		}, models.UpsertWorkOfferFromJSON)
	case "article.getArticlesPaginated":
		return withFallback(ctx, s, db, method, input, func() (json.RawMessage, error) {
			return handleArticles(db, input)
		}, models.UpsertArticleFromJSON)
	case "transaction.getPaginatedTransactions":
		return withFallback(ctx, s, db, method, input, func() (json.RawMessage, error) {
			return handleTransactions(db, input)
		}, models.UpsertTransactionFromJSON)
	}

	return cachedRequest(c, method, input, func() (json.RawMessage, error) {
		return s.Request(ctx, method, input)
	})
}

func handleEvents(db *gorm.DB, input json.RawMessage) (json.RawMessage, error) {
	var in struct {
		Limit      int      `json:"limit"`
		Cursor     string   `json:"cursor"`
		CountryID  string   `json:"countryId"`
		EventTypes []string `json:"eventTypes"`
	}
	json.Unmarshal(input, &in)

	result, err := models.QueryEvents(db, models.EventQuery{
		Limit:      in.Limit,
		Cursor:     in.Cursor,
		CountryID:  in.CountryID,
		EventTypes: in.EventTypes,
	})
	if err != nil {
		return nil, err
	}

	return buildTRPCResponse(result.Data, result.Cursor)
}

func handleWorkOffers(db *gorm.DB, input json.RawMessage) (json.RawMessage, error) {
	var in struct {
		Limit      int    `json:"limit"`
		Cursor     string `json:"cursor"`
		UserID     string `json:"userId"`
		RegionID   string `json:"regionId"`
		Energy     int    `json:"energy"`
		Production int    `json:"production"`
	}
	json.Unmarshal(input, &in)

	result, err := models.QueryWorkOffers(db, models.WorkOfferQuery{
		Limit:      in.Limit,
		Cursor:     in.Cursor,
		UserID:     in.UserID,
		RegionID:   in.RegionID,
		Energy:     in.Energy,
		Production: in.Production,
	})
	if err != nil {
		return nil, err
	}

	return buildTRPCResponse(result.Data, result.Cursor)
}

func handleArticles(db *gorm.DB, input json.RawMessage) (json.RawMessage, error) {
	var in struct {
		Type              string   `json:"type"`
		Limit             int      `json:"limit"`
		Cursor            string   `json:"cursor"`
		UserID            string   `json:"userId"`
		Categories        []string `json:"categories"`
		Languages         []string `json:"languages"`
		PositiveScoreOnly bool     `json:"positiveScoreOnly"`
	}
	json.Unmarshal(input, &in)

	result, err := models.QueryArticles(db, models.ArticleQuery{
		Type:              in.Type,
		Limit:             in.Limit,
		Cursor:            in.Cursor,
		UserID:            in.UserID,
		Categories:        in.Categories,
		Languages:         in.Languages,
		PositiveScoreOnly: in.PositiveScoreOnly,
	})
	if err != nil {
		return nil, err
	}

	return buildTRPCResponse(result.Data, result.Cursor)
}

func handleTransactions(db *gorm.DB, input json.RawMessage) (json.RawMessage, error) {
	var in struct {
		Limit           int    `json:"limit"`
		Cursor          string `json:"cursor"`
		UserID          string `json:"userId"`
		MuID            string `json:"muId"`
		CountryID       string `json:"countryId"`
		PartyID         string `json:"partyId"`
		ItemCode        string `json:"itemCode"`
		TransactionType string `json:"transactionType"`
	}
	json.Unmarshal(input, &in)

	result, err := models.QueryTransactions(db, models.TransactionQuery{
		Limit:           in.Limit,
		Cursor:          in.Cursor,
		UserID:          in.UserID,
		MuID:            in.MuID,
		CountryID:       in.CountryID,
		PartyID:         in.PartyID,
		ItemCode:        in.ItemCode,
		TransactionType: in.TransactionType,
	})
	if err != nil {
		return nil, err
	}

	return buildTRPCResponse(result.Data, result.Cursor)
}
