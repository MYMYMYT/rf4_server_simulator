package gameserver

import (
	"context"
	"fmt"

	"rf4server/internal/proto"
	"rf4server/internal/store"
)

func nativeInventoryContainers(kind uint64) []string {
	switch kind {
	case containerBackpack:
		return []string{"backpack", "rig"}
	case 17:
		return []string{fmt.Sprintf("native:%d", kind)}
	case containerTradingCards:
		// Album cards remain in the collection inventory; their parent GUID
		// determines which album slot the client displays them in.
		return []string{tradingCardContainer, "card_deck"}
	case 10, 11:
		// 房屋仓库（容器枚举 10/11 统一落 item.container='house'，与
		// containerEnumName 一致）。缺这条映射时 4/1 计数与 4/4 列表
		// 对房屋容器一律回空，客户端房屋仓库 UI 永远无数据。
		return []string{"house"}
	}
	return nil
}

// 导入快照保留渔获资料，容器和版本以当前护网为准。
// 物种稀有度（f4/f24 稀有位）始终以静态表为准——旧快照可能缺失或为 0。
func (s *Server) keepnetItemTree(ctx context.Context, entry *store.KeepnetEntry, level string) *proto.Value {
	rarity := s.DB.SpeciesRarity(ctx, entry.Species)
	s.fillKeepnetVariant(entry)
	row, err := s.DB.GetItemByGUID(ctx, entry.EchoGUID)
	if err == nil && row.UID == entry.UID && row.Container == "keepnet" && row.TypeID == 25 && row.WireClass == "ecknbkpeeji" {
		if fields, err := FieldsFromJSON(row.FieldsJSON); err == nil && len(fields) == 29 {
			fields[4].Num = ptrU64(uint64(rarity))
			// 25.f24 的稀有位同样以静态表为准：旧快照没有这一位，
			// 普通鱼快照也可能残留错误稀有位，两条都要覆盖。
			state := uint64(0)
			if fields[24].Num != nil {
				state = *fields[24].Num
			}
			if rarity > 0 {
				state |= uint64(keepnetStateRare)
			} else {
				state &^= uint64(keepnetStateRare)
			}
			fields[24].Num = ptrU64(state)
			fields[16].Hex = entry.EchoGUID
			fields[18].Num = ptrU64(containerKeepnet)
			fields[22].Num = ptrU64(uint64(netTicksNow()))
			// f11 = 模型族名（与 27.f22 同名 oajeeklppia）；命名鱼的名字挪到 f12。
			// 老存档里 f11 存的是名字，这里必须按当前推导值刷新，否则历史鱼获
			// 会跟新钓的鱼用不同的模型。
			switch {
			case entry.ModelVariant != "":
				model := entry.ModelVariant
				fields[11].Str = &model
				if entry.NameZh != "" {
					name := entry.NameZh
					fields[12].Str = &name
				}
			case entry.NameZh != "":
				name := entry.NameZh
				fields[11].Str = &name
				fields[12].Str = nil
			default:
				fields[11].Str = nil
				fields[12].Str = nil
			}
			return proto.Obj(row.WireClass, uint32(row.TypeID), fields...)
		}
	}
	return buildKeepnet25(entry, level, rarity, entry.ModelVariant)
}

func inventoryContainerMatches(name string, allowed []string) bool {
	for _, value := range allowed {
		if name == value {
			return true
		}
	}
	return false
}
