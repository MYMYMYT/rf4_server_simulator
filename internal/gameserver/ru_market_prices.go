package gameserver

import (
	_ "embed"
	"encoding/json"
	"math"
	"sort"
	"sync"
	"time"
)

// 俄服（RU）官服鱼市价格模型。
//
// 客户端 25.f3 的档位（dohoaehalpj）与官服阈值一致，共 4 档：
//   1 = 未达标（低于 Record / 绿旗，Under/undersized）
//   2 = 达标（Record / 绿旗，Valuable；官服 RU 叫 Зачётная рыба）
//   3 = 星标 / 奖杯（Trophy / 黄旗）
//   4 = 稀有 / 超级奖杯（Super Trophy / 青旗，蓝冠；可命名鱼）
//
// 鱼市收购价 = 该档位每公斤银币价 × 重量(kg)，四舍五入到分（1 银 = 100 分）。
// 每公斤价来自 assets/ru_market_prices.json（按地图 × 鱼种 × 档位存银币/公斤）：
//   - 档位缺失时按 grade_multipliers 从同图同鱼种其他档位折算；
//   - 地图缺失时按 map_market_multipliers.json 的地图倍率从其他地图折算；
//   - 全缺时用 defaults_silver_per_kg 兜底。
// 稀有度（species_rarity）不额外乘价：稀有鱼的单价已经体现在鱼种价格表里，
// 官方稀有度标记只影响图鉴/播报，不影响鱼市单价。
// 官服鱼市没有随时间腐败降价；freshness_enabled=false 时忽略新鲜度系数。

//go:embed assets/ru_market_prices.json
var ruMarketPricesJSON []byte

type ruPriceRules struct {
	SchemaVersion         int                                      `json:"schema_version"`
	Unit                  string                                   `json:"unit"`
	GradeMultipliers      map[string]float64                       `json:"grade_multipliers"`
	UndersizedOfQualified *float64                                 `json:"undersized_of_qualified"`
	BlueCrownOfTrophy     *float64                                 `json:"blue_crown_of_trophy"`
	FreshnessEnabled      *bool                                    `json:"freshness_enabled"`
	DefaultsSilverPerKg   map[string]float64                       `json:"defaults_silver_per_kg"`
	ByMap                 map[string]map[string]map[string]float64 `json:"by_map"`
}

var (
	ruPricesOnce sync.Once
	ruPricesData *ruPriceRules
)

const (
	ruGradeUndersized = int64(1) // 未达标
	ruGradeValuable   = int64(2) // 达标
	ruGradeTrophy     = int64(3) // 奖杯 / 星标
	ruGradeSuper      = int64(4) // 超级奖杯 / 稀有 / 蓝冠
)

// 默认档位倍率（以“达标”档为 1.0）。
// 依据：rf4db 全鱼种统计里只有两条恒定关系——电视线=2×达标、蓝冠=1.5×星级；
// 星级/达标 因鱼种而异（2~14x），所以优先用表里的实测星级价，倍率只作兜底。
// 未达标官方只给“象征性金额”，默认取达标价的 10%（可在 JSON 改 undersized_of_qualified）。
var ruDefaultGradeMultipliers = map[string]float64{"1": 0.1, "2": 1.0, "3": 2.0, "4": 3.0}

func loadRUPriceRules() *ruPriceRules {
	ruPricesOnce.Do(func() {
		r := &ruPriceRules{}
		if err := json.Unmarshal(ruMarketPricesJSON, r); err != nil {
			ruPricesData = nil
			return
		}
		ruPricesData = r
	})
	return ruPricesData
}

func ruGradeKey(grade int64) string {
	switch {
	case grade <= ruGradeUndersized:
		return "1"
	case grade == ruGradeValuable:
		return "2"
	case grade == ruGradeTrophy:
		return "3"
	default:
		return "4"
	}
}

func (r *ruPriceRules) gradeMultiplier(grade int64) float64 {
	if r != nil {
		if v, ok := r.GradeMultipliers[ruGradeKey(grade)]; ok && v > 0 {
			return v
		}
	}
	return ruDefaultGradeMultipliers[ruGradeKey(grade)]
}

// undersizedRatio 未达标鱼相对达标价的折扣（官方“象征性金额”，默认 0.1）。
func (r *ruPriceRules) undersizedRatio() float64 {
	if r != nil && r.UndersizedOfQualified != nil && *r.UndersizedOfQualified > 0 {
		return *r.UndersizedOfQualified
	}
	return 0.1
}

// blueCrownRatio 蓝冠相对星级价的倍率（rf4db 全样本恒为 1.5）。
func (r *ruPriceRules) blueCrownRatio() float64 {
	if r != nil && r.BlueCrownOfTrophy != nil && *r.BlueCrownOfTrophy > 0 {
		return *r.BlueCrownOfTrophy
	}
	return 1.5
}

// tablePricePerKg 只查价格表：同图同鱼种优先精确档位，否则按倍率从其他档位折算。
func (r *ruPriceRules) tablePricePerKg(mapKey, fishKey string, grade int64) float64 {
	if r == nil || mapKey == "" || fishKey == "" {
		return 0
	}
	fishes := r.ByMap[mapKey]
	if fishes == nil {
		return 0
	}
	rec := fishes[fishKey]
	if rec == nil {
		return 0
	}
	// rf4db 列与档位对应：达标=2(grade2)、星级=3(grade3)、蓝冠=4(grade4)。
	// 未达标(grade1) 官方只给象征性金额，按达标价乘 undersized_of_qualified。
	qualified := rec["2"]
	switch ruGradeKey(grade) {
	case "1":
		if v := rec["1"]; v > 0 {
			return v
		}
		if qualified > 0 {
			return qualified * r.undersizedRatio()
		}
		return 0
	case "2":
		if qualified > 0 {
			return qualified
		}
	case "3":
		if v := rec["3"]; v > 0 {
			return v
		}
		if qualified > 0 {
			return qualified * r.gradeMultiplier(ruGradeTrophy)
		}
		if v := rec["4"]; v > 0 {
			return v / r.blueCrownRatio()
		}
		return 0
	case "4":
		if v := rec["4"]; v > 0 {
			return v
		}
		if v := rec["3"]; v > 0 {
			return v * r.blueCrownRatio()
		}
		if qualified > 0 {
			return qualified * r.gradeMultiplier(ruGradeSuper)
		}
		return 0
	}
	// 兜底：从任意已有档位按倍率折算（防御异常数据）。
	base := r.gradeMultiplier(grade)
	for _, og := range []int64{ruGradeValuable, ruGradeTrophy, ruGradeSuper} {
		ok := ruGradeKey(og)
		v := rec[ok]
		if v <= 0 {
			continue
		}
		m := r.gradeMultiplier(og)
		if m <= 0 {
			continue
		}
		return v * base / m
	}
	return 0
}

// ruPricePerKg 返回某地图、鱼种、档位的官服鱼市价（银币/公斤）。
func ruPricePerKg(mapKey, fishKey string, grade int64) float64 {
	r := loadRUPriceRules()
	if r == nil {
		return 0
	}
	if v := r.tablePricePerKg(mapKey, fishKey, grade); v > 0 {
		return v
	}
	if fishKey == "" {
		return 0
	}
	maps := ruSortedMapKeys(r)
	targetPct := marketMapPct(mapKey)
	for _, baseMap := range maps {
		if baseMap == mapKey {
			continue
		}
		v := r.tablePricePerKg(baseMap, fishKey, grade)
		if v <= 0 {
			continue
		}
		if targetPct <= 0 {
			return v
		}
		basePct := marketMapPct(baseMap)
		if basePct <= 0 {
			continue
		}
		return v / basePct * targetPct
	}
	return 0
}

// ruSortedMapKeys 返回价格表的地图键（排序，保证跨图折算的基准地图确定）。
func ruSortedMapKeys(r *ruPriceRules) []string {
	if r == nil {
		return nil
	}
	maps := make([]string, 0, len(r.ByMap))
	for m := range r.ByMap {
		maps = append(maps, m)
	}
	sort.Strings(maps)
	return maps
}

// ruDefaultPricePerKg 未标定鱼种的兜底每公斤价。
func ruDefaultPricePerKg(grade int64) float64 {
	r := loadRUPriceRules()
	if r != nil {
		if v, ok := r.DefaultsSilverPerKg[ruGradeKey(grade)]; ok && v > 0 {
			return v
		}
	}
	return 20.0 * ruDefaultGradeMultipliers[ruGradeKey(grade)]
}

// ruFreshnessEnabled 官服是否有入护腐败降价；未配置时保持旧行为。
func ruFreshnessEnabled() bool {
	r := loadRUPriceRules()
	if r == nil || r.FreshnessEnabled == nil {
		return true
	}
	return *r.FreshnessEnabled
}

// ruFishPriceCents 计算单条鱼的官服口径鱼市报价（银币分）。
func ruFishPriceCents(fishKey string, grade, weightG int64, mapKey string, tsCaught, now float64) int64 {
	if weightG < 1 {
		weightG = 1
	}
	perKg := ruPricePerKg(mapKey, fishKey, grade)
	if perKg <= 0 {
		perKg = ruDefaultPricePerKg(grade)
	}
	if perKg <= 0 {
		return 0
	}
	cents := perKg * float64(weightG) / 1000.0 * 100.0
	if ruFreshnessEnabled() {
		cents *= freshnessFactor(tsCaught, now)
	}
	if cents < 1 {
		return 1
	}
	return int64(math.Round(cents))
}

// ruCatchSilver 渔获结算/入护展示用整银价格（与鱼市报价同一张表，四舍五入到 1 银）。
func ruCatchSilver(fishKey string, grade, weightG int64, locLevel string, tsCaught float64) int64 {
	cents := ruFishPriceCents(fishKey, grade, weightG, locLevel, tsCaught, float64(time.Now().Unix()))
	if cents <= 0 {
		return 0
	}
	return int64(math.Round(float64(cents) / 100.0))
}
