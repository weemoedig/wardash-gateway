package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	gocache "github.com/patrickmn/go-cache"
)

type cacheKeyFunc func(json.RawMessage) (string, bool)

type cacheConfig struct {
	ttl     time.Duration
	keyFunc cacheKeyFunc
}

func staticKey() cacheKeyFunc {
	return func(_ json.RawMessage) (string, bool) { return "", true }
}

func singleField(field string) cacheKeyFunc {
	return func(input json.RawMessage) (string, bool) {
		var m map[string]json.RawMessage
		err := json.Unmarshal(input, &m)
		if err != nil {
			return "", false
		}
		raw, ok := m[field]
		if !ok {
			return "", false
		}
		value, ok := jsonScalarKey(raw)
		if !ok {
			return "", false
		}
		return fieldPart(field, value), true
	}
}

func multiField(fields ...string) cacheKeyFunc {
	return func(input json.RawMessage) (string, bool) {
		var m map[string]json.RawMessage
		err := json.Unmarshal(input, &m)
		if err != nil {
			return "", false
		}
		parts := make([]string, len(fields))
		for i, field := range fields {
			raw, ok := m[field]
			if !ok {
				return "", false
			}
			value, ok := jsonScalarKey(raw)
			if !ok {
				return "", false
			}
			parts[i] = fieldPart(field, value)
		}
		return strings.Join(parts, ":"), true
	}
}

func jsonScalarKey(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return "", false
		}
		return s, true
	}

	var v any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&v); err != nil {
		return "", false
	}

	switch value := v.(type) {
	case bool:
		return fmt.Sprintf("%t", value), true
	case json.Number:
		if value == "" {
			return "", false
		}
		return value.String(), true
	default:
		return "", false
	}
}

func fieldPart(field string, value string) string {
	return url.QueryEscape(field) + "=" + url.QueryEscape(value)
}

func credentialCacheScope(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:8])
}

var cacheConfigs = map[string]cacheConfig{
	"company.getById":            {5 * time.Minute, singleField("companyId")},
	"company.getProductionBonus": {5 * time.Minute, singleField("companyId")},
	"company.getRecommendedRegionIdsByItemCode": {
		5 * time.Minute,
		multiField("itemCode", "count"),
	},
	"country.getCountryById":    {5 * time.Minute, singleField("countryId")},
	"country.getAllCountries":   {5 * time.Minute, staticKey()},
	"alliance.getById":          {5 * time.Minute, singleField("allianceId")},
	"government.getByCountryId": {5 * time.Minute, singleField("countryId")},
	"region.getById":            {5 * time.Minute, singleField("regionId")},
	"region.getAll":             {5 * time.Minute, staticKey()},
	"region.getRegionsObject":   {5 * time.Minute, staticKey()},
	"battle.getById":            {5 * time.Minute, singleField("battleId")},
	"battleLootSummary.getByBattleAndUser": {
		2 * time.Minute,
		multiField("battleId", "userId"),
	},
	"round.getById":                {5 * time.Minute, singleField("roundId")},
	"itemTrading.getPrices":        {10 * time.Minute, staticKey()},
	"itemOffer.getById":            {5 * time.Minute, singleField("itemOfferId")},
	"workOffer.getById":            {5 * time.Minute, singleField("workOfferId")},
	"ranking.getRanking":           {5 * time.Minute, singleField("rankingType")},
	"gameConfig.getDates":          {5 * time.Minute, staticKey()},
	"gameConfig.getGameConfig":     {5 * time.Minute, staticKey()},
	"user.getUserLite":             {5 * time.Minute, singleField("userId")},
	"user.getUserById":             {5 * time.Minute, singleField("userId")},
	"article.getArticleById":       {5 * time.Minute, singleField("articleId")},
	"article.getArticleLiteById":   {5 * time.Minute, singleField("articleId")},
	"mu.getById":                   {5 * time.Minute, singleField("muId")},
	"party.getById":                {5 * time.Minute, singleField("partyId")},
	"tournament.getById":           {5 * time.Minute, singleField("tournamentId")},
	"tournament.getLastTournament": {5 * time.Minute, staticKey()},
	"tournamentTeam.getById":       {5 * time.Minute, singleField("tournamentTeamId")},
	"tournamentTeam.getByTournamentId": {
		5 * time.Minute,
		singleField("tournamentId"),
	},
	"war.getById":                     {5 * time.Minute, singleField("warId")},
	"worker.getTotalWorkersCount":     {5 * time.Minute, singleField("userId")},
	"worker.getWorkers":               {5 * time.Minute, multiField("companyId", "userId")},
	"inventory.fetchCurrentEquipment": {5 * time.Minute, singleField("userId")},
	"battleRanking.getRanking":        {2 * time.Minute, multiField("battleId", "roundId", "warId", "dataType", "type", "side")},
}

func cachedRequest(
	c *gocache.Cache,
	method string,
	input json.RawMessage,
	apiKey string,
	fetch func() (json.RawMessage, error),
) (json.RawMessage, error) {
	cfg, ok := cacheConfigs[method]
	if !ok {
		return fetch()
	}

	derived, ok := cfg.keyFunc(input)
	if !ok {
		return fetch()
	}

	key := method + ":credential=" + credentialCacheScope(apiKey)
	if derived != "" {
		key += ":" + derived
	}

	cached, found := c.Get(key)
	if found {
		return cached.(json.RawMessage), nil
	}

	result, err := fetch()
	if err != nil {
		return nil, err
	}

	c.Set(key, result, cfg.ttl)
	return result, nil
}
