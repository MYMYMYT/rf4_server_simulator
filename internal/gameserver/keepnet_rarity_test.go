package gameserver

import (
	"context"
	"testing"

	"rf4server/internal/proto"
	"rf4server/internal/store"
	"rf4server/internal/wiregen"
)

// TestKeepnet25RarityField 25.f4 物种稀有度取自静态表 species_rarity，
// 结算卡 tag5 与图鉴按它渲染紫/粉标（jmpghlofkoc：0 普通 1 稀有 2 超稀有）。
// 25.f24 的稀有位（bapnijnooje bit0）供鱼市/鱼护物品卡画紫色稀有标。
func TestKeepnet25RarityField(t *testing.T) {
	s, db := newProgressSrv(t)
	if _, err := db.SQL.Exec(`INSERT INTO species_rarity(fish_key, rarity) VALUES('asp',2),('perch',1)`); err != nil {
		t.Fatal(err)
	}
	if got := s.rarityOf25("asp"); got != 2 {
		t.Fatalf("rarityOf25(asp)=%d want 2", got)
	}
	if got := s.rarityOf25("perch"); got != 1 {
		t.Fatalf("rarityOf25(perch)=%d want 1", got)
	}
	if got := s.rarityOf25("unknown_fish"); got != 0 {
		t.Fatalf("rarityOf25(unknown)=%d want 0（缺条目视为普通）", got)
	}

	e := &store.KeepnetEntry{EchoGUID: "22222222222222222222222222222222", Species: "asp", WeightG: 1200, LengthM: 0.55, Cat: 3, KeepLevel: "level_002_kirzah_river"}
	tree := buildKeepnet25(e, "", s.rarityOf25(e.Species), "")
	if got := u32Of(tree.Field(4).Num); got != 2 {
		t.Fatalf("25.f4=%d want 2", got)
	}
	if got := u32Of(tree.Field(24).Num); got&1 != 1 {
		t.Fatalf("25.f24=%#x 缺稀有位（bit0）", got)
	}

	// 普通鱼种不能带稀有位，否则鱼市/鱼护的物品卡会给普通鱼挂紫标。
	plain := &store.KeepnetEntry{EchoGUID: "33333333333333333333333333333333", Species: "unknown_fish", WeightG: 300, LengthM: 0.3, Cat: 1}
	plainTree := buildKeepnet25(plain, "", s.rarityOf25(plain.Species), "")
	if got := u32Of(plainTree.Field(24).Num); got&1 != 0 {
		t.Fatalf("普通鱼 25.f24=%#x 不应带稀有位", got)
	}

	// 蓝冠未命名可命名位（1<<22）与稀有位必须共存，不能互相覆盖。
	blue := &store.KeepnetEntry{EchoGUID: "44444444444444444444444444444444", Species: "asp", WeightG: 2400, LengthM: 0.7, Cat: 4}
	blueTree := buildKeepnet25(blue, "", s.rarityOf25(blue.Species), "")
	if got := u32Of(blueTree.Field(24).Num); got&1 != 1 || got&(1<<22) == 0 {
		t.Fatalf("蓝冠稀有鱼 25.f24=%#x 需同时带稀有位与可命名位", got)
	}

	// wire 往返：f4 必须按 BYTE 落在字节流正确槽位（客户端 plphdfehkjl 顺序读取）。
	body, err := proto.EncodeValue(tree, wiregen.Table())
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, err := proto.DecodeValue(body, 0, wiregen.Table())
	if err != nil {
		t.Fatal(err)
	}
	f4 := decoded.Field(4)
	if f4.Op != "BYTE" || u32Of(f4.Num) != 2 {
		t.Fatalf("往返后 25.f4 op=%s num=%v want BYTE/2", f4.Op, f4.Num)
	}
}

// TestKeepnetItemTreeRarityOverride 快照路径的 f4 与 f24 稀有位以静态表为准——
// 导入快照可能缺失或带旧值，不能原样透传。
func TestKeepnetItemTreeRarityOverride(t *testing.T) {
	srv, _ := newEconomyTxTestServer(t)
	const uid int64 = 7358
	entry, _ := seedImportedKeepnetFish(t, srv, uid, 9, true)
	if _, err := srv.DB.SQL.Exec(`INSERT INTO species_rarity(fish_key, rarity) VALUES('asp',2)`); err != nil {
		t.Fatal(err)
	}
	tree := srv.keepnetItemTree(context.Background(), entry, entry.KeepLevel)
	if got := u32Of(tree.Field(4).Num); got != 2 {
		t.Fatalf("快照路径 25.f4=%d want 2（静态表覆盖）", got)
	}
	if got := u32Of(tree.Field(24).Num); got&1 != 1 {
		t.Fatalf("快照路径 25.f24=%#x 缺稀有位（静态表覆盖）", got)
	}
	// 静态表改回普通后，快照里的旧稀有位必须被清掉。
	if _, err := srv.DB.SQL.Exec(`DELETE FROM species_rarity WHERE fish_key='asp'`); err != nil {
		t.Fatal(err)
	}
	tree = srv.keepnetItemTree(context.Background(), entry, entry.KeepLevel)
	if got := u32Of(tree.Field(24).Num); got&1 != 0 {
		t.Fatalf("普通鱼快照路径 25.f24=%#x 不应保留稀有位", got)
	}
}
