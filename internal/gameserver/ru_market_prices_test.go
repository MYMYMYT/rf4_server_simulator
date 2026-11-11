package gameserver

import (
	"math"
	"testing"
)

// TestRUPriceTableStructure 校验官服价格表本身可用：至少一张地图有数据，
// 所有价格为正，档位倍率为正，且未标定鱼种有兜底价。
func TestRUPriceTableStructure(t *testing.T) {
	r := loadRUPriceRules()
	if r == nil {
		t.Fatal("ru_market_prices.json 解析失败")
	}
	if len(r.ByMap) == 0 {
		t.Fatal("价格表为空")
	}
	count := 0
	for mapKey, fishes := range r.ByMap {
		if mapKey == "" {
			t.Error("空地图键")
		}
		for fishKey, rec := range fishes {
			if fishKey == "" {
				t.Error("空鱼种键")
			}
			for grade, v := range rec {
				count++
				if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
					t.Errorf("%s/%s 档位 %s 价格非法: %v", mapKey, fishKey, grade, v)
				}
			}
		}
	}
	if count == 0 {
		t.Fatal("价格表没有任何价格条目")
	}
	for _, g := range []int64{1, 2, 3, 4} {
		if v := r.gradeMultiplier(g); v <= 0 {
			t.Errorf("档位 %d 倍率非法: %v", g, v)
		}
		if v := ruDefaultPricePerKg(g); v <= 0 {
			t.Errorf("档位 %d 兜底价非法: %v", g, v)
		}
	}
}

// TestRUPriceSearchOrder 校验档位映射：达标=grade2、星级=grade3、蓝冠=grade4，
// 未达标=达标价×undersized_of_qualified，缺档按官方兜底倍率。
func TestRUPriceSearchOrder(t *testing.T) {
	r := loadRUPriceRules()
	if r == nil || len(r.ByMap) == 0 {
		t.Skip("价格表为空")
	}
	var mapKey, fishKey string
	var rec map[string]float64
	for m, fishes := range r.ByMap {
		for f, rr := range fishes {
			if rr["2"] > 0 {
				mapKey, fishKey, rec = m, f, rr
				break
			}
		}
		if fishKey != "" {
			break
		}
	}
	if fishKey == "" {
		t.Skip("价格表缺少达标档数据")
	}
	q := rec["2"]
	if got := r.tablePricePerKg(mapKey, fishKey, 1); math.Abs(got-q*r.undersizedRatio()) > 1e-6 {
		t.Errorf("未达标价 = %v want %v (达标 %v × %.3f)", got, q*r.undersizedRatio(), q, r.undersizedRatio())
	}
	if got := r.tablePricePerKg(mapKey, fishKey, 2); math.Abs(got-q) > 1e-6 {
		t.Errorf("达标价 = %v want %v", got, q)
	}
	wantTrophy := rec["3"]
	if wantTrophy <= 0 {
		wantTrophy = q * r.gradeMultiplier(3)
	}
	if got := r.tablePricePerKg(mapKey, fishKey, 3); math.Abs(got-wantTrophy) > 1e-6 {
		t.Errorf("奖杯价 = %v want %v", got, wantTrophy)
	}
	wantBlue := rec["4"]
	if wantBlue <= 0 && rec["3"] > 0 {
		wantBlue = rec["3"] * r.blueCrownRatio()
	}
	if wantBlue <= 0 {
		wantBlue = q * r.gradeMultiplier(4)
	}
	if got := r.tablePricePerKg(mapKey, fishKey, 4); math.Abs(got-wantBlue) > 1e-6 {
		t.Errorf("蓝冠价 = %v want %v", got, wantBlue)
	}
	// 未知鱼种没有表价，走兜底。
	if got := ruPricePerKg(mapKey, "__no_such_fish__", 2); got != 0 {
		t.Errorf("未知鱼种不应有表价: %v", got)
	}
	if got := ruDefaultPricePerKg(2); got <= 0 {
		t.Errorf("未知鱼种兜底价应为正: %v", got)
	}
}

// TestRUPriceGradeOrderAndScaling 校验同一尾鱼档位越高价越高，且地图倍率会生效。
func TestRUPriceGradeOrderAndScaling(t *testing.T) {
	const weight = 1000
	// 找一个至少覆盖 1/2/3 档的鱼种。
	r := loadRUPriceRules()
	if r == nil {
		t.Skip("价格表为空")
	}
	var mapKey, fishKey string
	for m, fishes := range r.ByMap {
		for f, rec := range fishes {
			if rec["2"] > 0 {
				mapKey, fishKey = m, f
				break
			}
		}
		if fishKey != "" {
			break
		}
	}
	if fishKey == "" {
		t.Skip("没有达标档数据的鱼种")
	}
	p1 := ruFishPriceCents(fishKey, 1, weight, mapKey, 0, 0)
	p2 := ruFishPriceCents(fishKey, 2, weight, mapKey, 0, 0)
	p3 := ruFishPriceCents(fishKey, 3, weight, mapKey, 0, 0)
	p4 := ruFishPriceCents(fishKey, 4, weight, mapKey, 0, 0)
	if !(p1 < p2 && p2 < p3 && p3 < p4) {
		t.Errorf("档位价应单调递增: %d %d %d %d (%s/%s)", p1, p2, p3, p4, mapKey, fishKey)
	}
	// 地图倍率：换一张没有该鱼数据的地图，价格应按 marketMapPct 折算。
	for _, other := range ruSortedMapKeys(r) {
		if other == mapKey {
			continue
		}
		if r.tablePricePerKg(other, fishKey, 2) > 0 {
			continue
		}
		otherPct := marketMapPct(other)
		if otherPct <= 0 {
			continue
		}
		// 与 ruPricePerKg 相同的基准图选择：排序后第一张有该鱼数据且倍率有效的图。
		var baseMap string
		var basePerKg float64
		for _, bm := range ruSortedMapKeys(r) {
			if bm == other {
				continue
			}
			v := r.tablePricePerKg(bm, fishKey, 2)
			if v <= 0 || marketMapPct(bm) <= 0 {
				continue
			}
			baseMap, basePerKg = bm, v
			break
		}
		if baseMap == "" {
			continue
		}
		basePct := marketMapPct(baseMap)
		if otherPct == basePct {
			continue
		}
		want := int64(math.Round(basePerKg / basePct * otherPct * weight / 1000.0 * 100.0))
		got := ruFishPriceCents(fishKey, 2, weight, other, 0, 0)
		if got != want {
			t.Errorf("跨图折算 = %d want %d (%s->%s)", got, want, baseMap, other)
		}
		return
	}
}
