package main

import (
	"context"
	"encoding/json"

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
		return handleEvents(db, input)
	case "workOffer.getWorkOffersPaginated":
		return handleWorkOffers(db, input)
	case "article.getArticlesPaginated":
		return handleArticles(db, input)
	case "transaction.getPaginatedTransactions":
		return handleTransactions(db, input)
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
