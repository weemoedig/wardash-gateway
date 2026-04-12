package main

import (
	"encoding/json"
	"strings"
	"time"

	gocache "github.com/patrickmn/go-cache"
)

type cacheConfig struct {
	ttl     time.Duration
	keyFunc func(json.RawMessage) string
}

func staticKey() func(json.RawMessage) string {
	return func(_ json.RawMessage) string { return "" }
}

func singleField(field string) func(json.RawMessage) string {
	return func(input json.RawMessage) string {
		var m map[string]json.RawMessage
		err := json.Unmarshal(input, &m)
		if err != nil {
			return ""
		}
		var v string
		raw, ok := m[field]
		if ok {
			json.Unmarshal(raw, &v)
		}
		return v
	}
}

func multiField(fields ...string) func(json.RawMessage) string {
	return func(input json.RawMessage) string {
		var m map[string]json.RawMessage
		err := json.Unmarshal(input, &m)
		if err != nil {
			return ""
		}
		parts := make([]string, len(fields))
		for i, field := range fields {
			var v string
			raw, ok := m[field]
			if ok {
				json.Unmarshal(raw, &v)
			}
			parts[i] = v
		}
		return strings.Join(parts, ":")
	}
}

var cacheConfigs = map[string]cacheConfig{
	"company.getById":                 {5 * time.Minute, singleField("companyId")},
	"country.getCountryById":          {5 * time.Minute, singleField("countryId")},
	"country.getAllCountries":         {5 * time.Minute, staticKey()},
	"government.getByCountryId":       {5 * time.Minute, singleField("countryId")},
	"region.getById":                  {5 * time.Minute, singleField("regionId")},
	"region.getRegionsObject":         {5 * time.Minute, staticKey()},
	"battle.getById":                  {5 * time.Minute, singleField("battleId")},
	"round.getById":                   {5 * time.Minute, singleField("roundId")},
	"itemTrading.getPrices":           {10 * time.Minute, staticKey()},
	"itemOffer.getById":               {5 * time.Minute, singleField("itemOfferId")},
	"workOffer.getById":               {5 * time.Minute, singleField("workOfferId")},
	"ranking.getRanking":              {5 * time.Minute, singleField("rankingType")},
	"gameConfig.getDates":             {5 * time.Minute, staticKey()},
	"gameConfig.getGameConfig":        {5 * time.Minute, staticKey()},
	"user.getUserLite":                {5 * time.Minute, singleField("userId")},
	"user.getUserById":                {5 * time.Minute, singleField("userId")},
	"article.getArticleById":          {5 * time.Minute, singleField("articleId")},
	"article.getArticleLiteById":      {5 * time.Minute, singleField("articleId")},
	"mu.getById":                      {5 * time.Minute, singleField("muId")},
	"worker.getTotalWorkersCount":     {5 * time.Minute, singleField("userId")},
	"worker.getWorkers":               {5 * time.Minute, multiField("companyId", "userId")},
	"inventory.fetchCurrentEquipment": {5 * time.Minute, singleField("userId")},
	"battleRanking.getRanking":        {2 * time.Minute, multiField("battleId", "roundId", "warId", "dataType", "type", "side")},
}

func cachedRequest(
	c *gocache.Cache,
	method string,
	input json.RawMessage,
	fetch func() (json.RawMessage, error),
) (json.RawMessage, error) {
	cfg, ok := cacheConfigs[method]
	if !ok {
		return fetch()
	}

	key := method
	derived := cfg.keyFunc(input)
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
