package gameserver

import (
	"context"
	"testing"
	"time"

	"rf4server/internal/proto"
	"rf4server/internal/store"
)

// 同一批鱼在不同水域卖出，价格必须随水域变化：有该鱼实测价的地图用实测价，
// 没有实测价的地图（如挪威海）按 marketMapPct 水域倍率从基准图折算。
// 实测价随鱼种不同不保证固定排序，因此这里验证“取价为正且随地图不同”，
// 倍率折算的具体公式由 TestRUPriceGradeOrderAndScaling 覆盖。
func TestMarketPriceFollowsSellMap(t *testing.T) {
	e := &store.KeepnetEntry{Species: "bream", Cat: 3, WeightG: 6054}
	now := 1750000000.0
	prices := map[string]int64{}
	for _, mk := range []string{
		"level_001_lake",            // 有 bream 实测价
		"level_004_torfyanoe_ozero", // 有 bream 实测价
		"level_012_north_sea",       // 无 bream 数据，走倍率折算
	} {
		p := marketPriceCentsForMap(e, now, mk)
		if p <= 0 {
			t.Fatalf("%s 价格应为正: %d", mk, p)
		}
		prices[mk] = p
	}
	distinct := map[int64]bool{}
	for _, p := range prices {
		distinct[p] = true
	}
	if len(distinct) < 2 {
		t.Fatalf("不同水域价格应不同: %v", prices)
	}
}

// 20/1 与 20/2 必须按玩家当前地图取价，而不是按鱼的 keep_loc。
// 这里让鱼的钓获地图与当前地图相反，并同时核对实际入账金额。
func TestMarketRateAndSellFollowCurrentMap(t *testing.T) {
	srv, db := newEconomyTxTestServer(t)
	const uid = int64(7201)
	ctx := context.Background()
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}
	now := float64(time.Now().Unix())
	insert := func(guid, keepLoc, keepLevel string) {
		t.Helper()
		if _, err := db.SQL.Exec(`INSERT INTO keepnet_items(echo_guid,uid,species,name_zh,weight_g,length_m,cat,keep_loc,keep_spot,keep_bait,keep_level,ts)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, guid, uid, "bream", "欧鳊", 6054, 0.2, 3, keepLoc, 1, "worm", keepLevel, now); err != nil {
			t.Fatal(err)
		}
	}
	ratePrice := func(guid string) int64 {
		t.Helper()
		body, ok := hMarketRate(economyTestCtx(srv, uid, proto.Args524(proto.Guid(guid))))
		if !ok {
			t.Fatal("20/1 未处理")
		}
		v, _, err := proto.DecodeValue(body, 0, srv.Table())
		if err != nil || v.Kind != proto.KindArgs || len(v.Items) != 1 || v.Items[0].Field(1).Num == nil {
			t.Fatalf("20/1 响应无效: %+v err=%v", v, err)
		}
		return int64(*v.Items[0].Field(1).Num)
	}
	sellPrice := func(guid string) int64 {
		t.Helper()
		before, err := db.GetProfile(ctx, uid)
		if err != nil {
			t.Fatal(err)
		}
		body, ok := hMarketSell(economyTestCtx(srv, uid, proto.Args524(proto.Guid(guid))))
		if !ok || len(body) == 0 || body[0] == 0x0F {
			t.Fatalf("20/2 未成功: ok=%v body=%x", ok, body)
		}
		after, err := db.GetProfile(ctx, uid)
		if err != nil {
			t.Fatal(err)
		}
		return after.Silver - before.Silver
	}

	const lakeFish = "d1000000000000000000000000000000"
	const seaFish = "d2000000000000000000000000000000"
	insert(lakeFish, "loc1_02", "level_001_lake")
	srv.setLevel(uid, "level_012_north_sea")
	e := &store.KeepnetEntry{Species: "bream", Cat: 3, WeightG: 6054}
	wantSea := marketPriceCentsForMap(e, now, "level_012_north_sea")
	if got := ratePrice(lakeFish); got != wantSea {
		t.Fatalf("海图询价=%d，期望卖鱼地图价 %d", got, wantSea)
	}
	if got := sellPrice(lakeFish); got != wantSea {
		t.Fatalf("海图入账=%d，期望询价 %d", got, wantSea)
	}

	insert(seaFish, "level_012_north_sea")
	srv.setLevel(uid, "level_001_lake")
	wantLake := marketPriceCentsForMap(e, now, "level_001_lake")
	if got := ratePrice(seaFish); got != wantLake {
		t.Fatalf("1 图询价=%d，期望卖鱼地图价 %d", got, wantLake)
	}
	if got := sellPrice(seaFish); got != wantLake {
		t.Fatalf("1 图入账=%d，期望询价 %d", got, wantLake)
	}
}
