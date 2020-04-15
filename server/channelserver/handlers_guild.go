package channelserver

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"github.com/Andoryuuta/Erupe/common/stringsupport"
	"github.com/Andoryuuta/Erupe/network/mhfpacket"
	"github.com/Andoryuuta/byteframe"
	"go.uber.org/zap"
	"sort"
)

func handleMsgMhfCreateGuild(s *Session, p mhfpacket.MHFPacket) {
	pkt := p.(*mhfpacket.MsgMhfCreateGuild)

	guildId, err := CreateGuild(s, stripNullTerminator(pkt.Name))

	if err != nil {
		bf := byteframe.NewByteFrame()

		// No reasoning behind these values other than they cause a 'failed to create'
		// style message, it's better than nothing for now.
		bf.WriteUint32(0x01010101)

		doAckSimpleFail(s, pkt.AckHandle, bf.Data())
		return
	}

	bf := byteframe.NewByteFrame()

	bf.WriteUint32(uint32(guildId))

	doAckSimpleSucceed(s, pkt.AckHandle, bf.Data())
}

func handleMsgMhfOperateGuild(s *Session, p mhfpacket.MHFPacket) {
	pkt := p.(*mhfpacket.MsgMhfOperateGuild)

	guild, err := GetGuildInfoByID(s, pkt.GuildID)

	if err != nil {
		return
	}

	characterGuildInfo, err := GetCharacterGuildData(s, s.charID)

	if err != nil {
		doAckSimpleFail(s, pkt.AckHandle, make([]byte, 4))
		return
	}

	bf := byteframe.NewByteFrame()

	switch pkt.Action {
	case mhfpacket.OPERATE_GUILD_ACTION_DISBAND:
		if guild.LeaderCharID != s.charID {
			s.logger.Warn(fmt.Sprintf("character '%d' is attempting to manage guild '%d' without permission", s.charID, guild.ID))
			return
		}

		err = guild.Disband(s)
		response := 0x01

		if err != nil {
			// All successful acks return 0x01, assuming 0x00 is failure
			response = 0x00
		}

		bf.WriteUint32(uint32(response))
	case mhfpacket.OPERATE_GUILD_ACTION_APPLY:
		err = guild.CreateApplication(s, s.charID, GuildApplicationTypeApplied, nil)

		if err != nil {
			// All successful acks return 0x01, assuming 0x00 is failure
			bf.WriteUint32(0x00)
		} else {
			bf.WriteUint32(guild.LeaderCharID)
		}
	case mhfpacket.OPERATE_GUILD_ACTION_LEAVE:
		var err error

		if characterGuildInfo.IsApplicant {
			err = guild.RejectApplication(s, s.charID)
		} else {
			err = guild.RemoveCharacter(s, s.charID)
		}

		response := 0x01

		if err != nil {
			// All successful acks return 0x01, assuming 0x00 is failure
			response = 0x00
		}

		bf.WriteUint32(uint32(response))
	case mhfpacket.OPERATE_GUILD_ACTION_DONATE:
		err := handleOperateGuildActionDonate(s, guild, pkt, bf)

		if err != nil {
			return
		}
	case mhfpacket.OPERATE_GUILD_SET_AVOID_LEADERSHIP_TRUE:
		handleAvoidLeadershipUpdate(s, pkt, true)
	case mhfpacket.OPERATE_GUILD_SET_AVOID_LEADERSHIP_FALSE:
		handleAvoidLeadershipUpdate(s, pkt, false)
	case mhfpacket.OPERATE_GUILD_ACTION_UPDATE_COMMENT:
		pbf := byteframe.NewByteFrameFromBytes(pkt.UnkData)

		if !characterGuildInfo.IsLeader && !characterGuildInfo.IsSubLeader() {
			doAckSimpleFail(s, pkt.AckHandle, make([]byte, 4))
			return
		}

		commentLength := pbf.ReadUint8()
		_ = pbf.ReadUint32()

		guild.Comment, err = stringsupport.ConvertShiftJISToUTF8(
			stripNullTerminator(string(pbf.ReadBytes(uint(commentLength)))),
		)

		if err != nil {
			s.logger.Warn("failed to convert guild comment to UTF8", zap.Error(err))
			doAckSimpleFail(s, pkt.AckHandle, make([]byte, 4))
			break
		}

		err = guild.Save(s)

		if err != nil {
			doAckSimpleFail(s, pkt.AckHandle, make([]byte, 4))
			return
		}

		bf.WriteUint32(0x00)
	case mhfpacket.OPERATE_GUILD_ACTION_UPDATE_MOTTO:
		if !characterGuildInfo.IsLeader && !characterGuildInfo.IsSubLeader() {
			doAckSimpleFail(s, pkt.AckHandle, make([]byte, 4))
			return
		}

		guild.SubMotto = pkt.UnkData[3]
		guild.MainMotto = pkt.UnkData[4]

		err := guild.Save(s)

		if err != nil {
			doAckSimpleFail(s, pkt.AckHandle, make([]byte, 4))
			return
		}
	default:
		panic(fmt.Sprintf("unhandled operate guild action '%d'", pkt.Action))
	}

	doAckSimpleSucceed(s, pkt.AckHandle, bf.Data())
}

func handleAvoidLeadershipUpdate(s *Session, pkt *mhfpacket.MsgMhfOperateGuild, avoidLeadership bool) {
	characterGuildData, err := GetCharacterGuildData(s, s.charID)

	if err != nil {
		doAckSimpleFail(s, pkt.AckHandle, make([]byte, 4))
		return
	}

	characterGuildData.AvoidLeadership = avoidLeadership

	err = characterGuildData.Save(s)

	if err != nil {
		doAckSimpleFail(s, pkt.AckHandle, make([]byte, 4))
		return
	}

	doAckSimpleSucceed(s, pkt.AckHandle, make([]byte, 4))
}

func handleOperateGuildActionDonate(s *Session, guild *Guild, pkt *mhfpacket.MsgMhfOperateGuild, bf *byteframe.ByteFrame) error {
	rp := binary.BigEndian.Uint16(pkt.UnkData[3:5])

	saveData, err := GetCharacterSaveData(s, s.charID)

	if err != nil {
		return err
	}

	if saveData.RP < rp {
		s.logger.Warn(
			"character attempting to donate more RP than they own",
			zap.Uint32("charID", s.charID),
			zap.Uint16("rp", rp),
		)
		return err
	}

	saveData.RP -= rp

	transaction, err := s.server.db.Begin()

	if err != nil {
		s.logger.Error("failed to start db transaction", zap.Error(err))
		return err
	}

	err = saveData.Save(s, transaction)

	if err != nil {
		err = transaction.Rollback()

		if err != nil {
			s.logger.Error("failed to rollback transaction", zap.Error(err))
		}

		return err
	}

	err = guild.DonateRP(s, rp, transaction)

	if err != nil {
		err = transaction.Rollback()

		if err != nil {
			s.logger.Error("failed to rollback transaction", zap.Error(err))
		}

		return err
	}

	err = transaction.Commit()

	if err != nil {
		s.logger.Error("failed to commit transaction", zap.Error(err))
		return err
	}

	bf.WriteUint32(uint32(saveData.RP)) // Points remaining

	return nil
}

func handleMsgMhfOperateGuildMember(s *Session, p mhfpacket.MHFPacket) {
	pkt := p.(*mhfpacket.MsgMhfOperateGuildMember)

	guild, err := GetGuildInfoByCharacterId(s, pkt.CharID)

	if err != nil || guild == nil {
		doAckSimpleFail(s, pkt.AckHandle, nil)
		return
		return
	}

	actorCharacter, err := GetCharacterGuildData(s, s.charID)

	if err != nil || (!actorCharacter.IsSubLeader() && guild.LeaderCharID != s.charID) {
		doAckSimpleFail(s, pkt.AckHandle, nil)
		return
	}

	if pkt.Action == mhfpacket.OPERATE_GUILD_MEMBER_ACTION_ACCEPT || pkt.Action == mhfpacket.OPERATE_GUILD_MEMBER_ACTION_REJECT {
		switch pkt.Action {
		case mhfpacket.OPERATE_GUILD_MEMBER_ACTION_ACCEPT:
			err = guild.AcceptApplication(s, pkt.CharID)
		case mhfpacket.OPERATE_GUILD_MEMBER_ACTION_REJECT:
			err = guild.RejectApplication(s, pkt.CharID)
		}

		if err != nil {
			doAckSimpleFail(s, pkt.AckHandle, nil)
		}

		doAckSimpleSucceed(s, pkt.AckHandle, nil)
		return
	}

	character, err := GetCharacterGuildData(s, pkt.CharID)

	if err != nil || character == nil {
		doAckSimpleFail(s, pkt.AckHandle, nil)
		return
	}

	switch pkt.Action {
	case mhfpacket.OPERATE_GUILD_MEMBER_ACTION_KICK:
		err = guild.RemoveCharacter(s, pkt.CharID)
	default:
		doAckSimpleFail(s, pkt.AckHandle, nil)
		panic(fmt.Sprintf("unhandled operateGuildMember action '%d'", pkt.Action))
	}

	if err != nil {
		doAckSimpleFail(s, pkt.AckHandle, nil)
		return
	}

	doAckSimpleSucceed(s, pkt.AckHandle, nil)
}

func handleMsgMhfInfoGuild(s *Session, p mhfpacket.MHFPacket) {
	pkt := p.(*mhfpacket.MsgMhfInfoGuild)

	var guild *Guild
	var err error

	if pkt.GuildID > 0 {
		guild, err = GetGuildInfoByID(s, pkt.GuildID)
	} else {
		guild, err = GetGuildInfoByCharacterId(s, s.charID)
	}

	if err == nil && guild != nil {
		guildName := stringsupport.MustConvertUTF8ToShiftJIS(guild.Name)
		guildComment := stringsupport.MustConvertUTF8ToShiftJIS(guild.Comment)

		characterGuildData, err := GetCharacterGuildData(s, s.charID)

		characterJoinedAt := uint32(0xFFFFFFFF)

		if characterGuildData != nil && characterGuildData.JoinedAt != nil {
			characterJoinedAt = uint32(characterGuildData.JoinedAt.Unix())
		}

		if err != nil {
			resp := byteframe.NewByteFrame()
			resp.WriteUint32(0) // Count
			resp.WriteUint8(0)  // Unk, read if count == 0.

			doAckBufSucceed(s, pkt.AckHandle, resp.Data())
			return
		}

		bf := byteframe.NewByteFrame()

		bf.WriteUint32(guild.ID)
		bf.WriteUint32(guild.LeaderCharID)
		// Unk 0x09 = Guild Hall available, maybe guild hall type?
		// Guild hall available on at least
		// 0x09 0x08 0x02
		// Should just be outright guild level for guild hall features, 17 gives everything
		bf.WriteUint16(guild.GuildHallType)
		bf.WriteUint16(guild.MemberCount)

		bf.WriteUint8(guild.MainMotto)
		bf.WriteUint8(guild.SubMotto)

		// Unk appears to be static
		bf.WriteBytes([]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})

		if characterGuildData == nil || characterGuildData.IsApplicant {
			bf.WriteUint16(0x00)
		} else if characterGuildData.IsSubLeader() || guild.LeaderCharID == s.charID {
			bf.WriteUint16(0x01)
		} else {
			bf.WriteUint16(0x02)
		}

		leaderName := stringsupport.MustConvertUTF8ToShiftJIS(guild.LeaderName) + "\x00"

		bf.WriteUint32(uint32(guild.CreatedAt.Unix()))
		bf.WriteUint32(characterJoinedAt)
		bf.WriteUint8(uint8(len(guildName)))
		bf.WriteUint8(uint8(len(guildComment)))
		bf.WriteUint8(uint8(5)) // Length of unknown string below
		bf.WriteUint8(uint8(len(leaderName)))
		bf.WriteBytes([]byte(guildName))
		bf.WriteBytes([]byte(guildComment))

		bf.WriteUint8(FestivalColourCodes[guild.FestivalColour])

		bf.WriteUint32(guild.RP)
		bf.WriteBytes([]byte(leaderName))
		//bf.WriteBytes([]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0x02, 0x00, 0x00, 0x00, 0x00}) // Unk
		bf.WriteBytes([]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x02, 0x02, 0x00, 0x00, 0x18, 0xBD}) // Level 17 guild's version

		// Pugi's names, probably expected as null until you have them with levels? Null gives them a default japanese name
		for i := 0; i < 3; i++ {
			bf.WriteUint8(0x1) // Name Length - 1 minimum due to null byte
			bf.WriteUint8(0x0) // Name string
		}

		// probably guild pugi properties, should be status, stamina and luck outfits
		bf.WriteBytes([]byte{
			0x04, 0x02, 0x03, 0x04, 0x02, 0x03, 0x00, 0x00, 0x00, 0x4E,
		})

		// Unk flags
		bf.WriteUint8(0x3C) // also seen as 0x32 on JP and 0x64 on TW

		bf.WriteBytes([]byte{
			0x00, 0x00, 0xD6, 0xD8, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		})

		bf.WriteUint32(0x00) // Alliance ID

		// TODO add alliance parts here
		//
		//if (AllianceID != 0) {
		//  uint16 AllianceDataUnk;
		//  uint16 AllianceDataUnk;
		//  uint16 AllianceNameLength;
		//	char AllianceName[AllianceNameLength];
		//
		//	byte NumAllianceMembers;
		//
		//	struct AllianceMember {
		//		uint32 Unk;
		//		uint32 Unk;
		//		uint16 Unk;
		//		uint16 Unk;
		//		uint16 Unk;
		//		uint16 GuildNameLength;
		//		char GuildName[GuildNameLength];
		//		uint16 GuildLeaderNameLength;
		//		char GuildLeaderName[GuildLeaderNameLength];
		//
		//	} member[NumAllianceMembers] <optimize=false>;
		//}

		applicants, err := GetGuildMembers(s, guild.ID, true)

		if err != nil {
			resp := byteframe.NewByteFrame()
			resp.WriteUint32(0) // Count
			resp.WriteUint8(5)  // Unk, read if count == 0.

			doAckBufSucceed(s, pkt.AckHandle, resp.Data())
		}

		bf.WriteUint16(uint16(len(applicants)))

		for _, applicant := range applicants {
			applicantName := stringsupport.MustConvertUTF8ToShiftJIS(applicant.Name) + "\x00"
			bf.WriteUint32(applicant.CharID)
			bf.WriteUint32(0x05)
			bf.WriteUint32(0x00320000)
			bf.WriteUint8(uint8(len(applicantName)))
			bf.WriteBytes([]byte(applicantName))
		}

		// This is guild icon data
		// temp canned bytes to avoid crashing when a guild has more than 1-2 members and a guild hall
		//bf.WriteBytes([]byte{0x00, 0x05, 0x00, 0x03, 0x00, 0x38, 0x01, 0x00, 0x01, 0xFF, 0xFF, 0xFF, 0x00, 0x04, 0x00, 0x03,
		//	0x00, 0x02, 0x00, 0x38, 0x01, 0x00, 0x03, 0xFF, 0xFF, 0xFF, 0x00, 0x69, 0x00, 0x60, 0x00, 0x01,
		//	0x00, 0x38, 0x01, 0x00, 0x00, 0xFF, 0xFF, 0xFF, 0x00, 0x6A, 0x00, 0x03, 0x00, 0x00, 0x00, 0x38,
		//	0x01, 0x00, 0x02, 0xFF, 0xFF, 0xFF, 0x00, 0x00, 0x00, 0x63, 0x00, 0x06, 0x00, 0x35, 0x01, 0x03,
		//	0x00, 0xFF, 0xFF, 0xFF, 0x00, 0x11, 0x00, 0x01, 0x00,
		//})

		// Unk bool? if true +3 bytes after this
		bf.WriteUint8(0x00)

		if guild.Icon != nil {
			bf.WriteUint16(uint16(len(guild.Icon.Parts)))

			for _, p := range guild.Icon.Parts {
				bf.WriteUint16(p.Index)
				bf.WriteUint16(p.ID)
				bf.WriteUint8(0x01)
				bf.WriteUint8(p.Size)
				bf.WriteUint8(p.Rotation)
				bf.WriteUint8(0xFF)
				bf.WriteUint16(0xFFFF)
				bf.WriteUint16(p.PosX)
				bf.WriteUint16(p.PosY)
			}
		} else {
			bf.WriteUint16(0x00)
		}

		doAckBufSucceed(s, pkt.AckHandle, bf.Data())
	} else {
		//// REALLY large/complex format... stubbing it out here for simplicity.
		//resp := byteframe.NewByteFrame()
		//resp.WriteUint32(0) // Count
		//resp.WriteUint8(0)  // Unk, read if count == 0.

		doAckBufSucceed(s, pkt.AckHandle, make([]byte, 8))
	}
}

func handleMsgMhfEnumerateGuild(s *Session, p mhfpacket.MHFPacket) {
	pkt := p.(*mhfpacket.MsgMhfEnumerateGuild)

	var guilds []*Guild
	var err error

	switch pkt.Type {
	case mhfpacket.ENUMERATE_GUILD_TYPE_NAME:
		// I have no idea if is really little endian, but it seems too weird to have a random static
		// 0x00 before the string
		searchTermLength := binary.LittleEndian.Uint16(pkt.RawDataPayload[9:11])
		searchTerm := pkt.RawDataPayload[11 : 11+searchTermLength]

		var searchTermSafe string

		searchTermSafe, err = stringsupport.ConvertShiftJISToUTF8(stripNullTerminator(string(searchTerm)))

		if err != nil {
			panic(err)
		}

		guilds, err = FindGuildsByName(s, searchTermSafe)
	default:
		panic(fmt.Sprintf("no handler for guild search type '%d'", pkt.Type))
	}

	if err != nil || guilds == nil {
		stubEnumerateNoResults(s, pkt.AckHandle)
		return
	}

	bf := byteframe.NewByteFrame()
	bf.WriteUint16(uint16(len(guilds)))

	for _, guild := range guilds {
		guildName := stringsupport.MustConvertUTF8ToShiftJIS(guild.Name)
		leaderName := stringsupport.MustConvertUTF8ToShiftJIS(guild.LeaderName)

		bf.WriteUint8(0x00) // Unk
		bf.WriteUint32(guild.ID)
		bf.WriteUint32(guild.LeaderCharID)
		bf.WriteUint16(guild.MemberCount)
		bf.WriteUint8(0x00)  // Unk
		bf.WriteUint8(0x00)  // Unk
		bf.WriteUint16(0x00) // Rank
		bf.WriteUint32(uint32(guild.CreatedAt.Unix()))
		bf.WriteUint8(uint8(len(guildName)))
		bf.WriteBytes([]byte(guildName))
		bf.WriteUint8(uint8(len(leaderName)))
		bf.WriteBytes([]byte(leaderName))
		bf.WriteUint8(0x01) // Unk
	}

	bf.WriteUint8(0x01) // Unk
	bf.WriteUint8(0x00) // Unk

	doAckBufSucceed(s, pkt.AckHandle, bf.Data())
}

func handleMsgMhfUpdateGuild(s *Session, p mhfpacket.MHFPacket) {}

func handleMsgMhfArrangeGuildMember(s *Session, p mhfpacket.MHFPacket) {
	pkt := p.(*mhfpacket.MsgMhfArrangeGuildMember)

	guild, err := GetGuildInfoByID(s, pkt.GuildID)

	if err != nil {
		s.logger.Error(
			"failed to respond to ArrangeGuildMember message",
			zap.Uint32("charID", s.charID),
		)
		return
	}

	if guild.LeaderCharID != s.charID {
		s.logger.Error("non leader attempting to rearrange guild members!",
			zap.Uint32("charID", s.charID),
			zap.Uint32("guildID", guild.ID),
		)
		return
	}

	err = guild.ArrangeCharacters(s, pkt.CharIDs)

	if err != nil {
		s.logger.Error(
			"failed to respond to ArrangeGuildMember message",
			zap.Uint32("charID", s.charID),
			zap.Uint32("guildID", guild.ID),
		)
		return
	}

	doAckSimpleSucceed(s, pkt.AckHandle, make([]byte, 4))
}

func handleMsgMhfEnumerateGuildMember(s *Session, p mhfpacket.MHFPacket) {
	pkt := p.(*mhfpacket.MsgMhfEnumerateGuildMember)

	var guild *Guild
	var err error

	if pkt.GuildID > 0 {
		guild, err = GetGuildInfoByID(s, pkt.GuildID)
	} else {
		guild, err = GetGuildInfoByCharacterId(s, s.charID)
	}

	if err != nil {
		s.logger.Warn("failed to retrieve guild sending no result message")
		doAckBufSucceed(s, pkt.AckHandle, make([]byte, 2))
		return
	} else if guild == nil {
		doAckBufSucceed(s, pkt.AckHandle, make([]byte, 2))
		return
	}

	guildMembers, err := GetGuildMembers(s, guild.ID, false)

	if err != nil {
		s.logger.Error("failed to retrieve guild")
		return
	}

	bf := byteframe.NewByteFrame()

	bf.WriteUint16(guild.MemberCount)

	sort.Slice(guildMembers[:], func(i, j int) bool {
		return guildMembers[i].OrderIndex < guildMembers[j].OrderIndex
	})

	for _, member := range guildMembers {
		name := stringsupport.MustConvertUTF8ToShiftJIS(member.Name) + "\x00"

		bf.WriteUint32(member.CharID)

		// Exp, HR[x] is split by 0, 1, 30, 50, 99, 299, 998, 999
		bf.WriteUint16(member.Exp) // Rank flags
		bf.WriteUint16(0x00)       // Grank
		bf.WriteUint16(0x00)       // Unk
		bf.WriteUint16(0x00)       // Some rank?
		bf.WriteUint8(member.OrderIndex)
		bf.WriteUint16(uint16(len(name)))
		bf.WriteBytes([]byte(name))
	}

	for _, member := range guildMembers {
		bf.WriteUint32(member.LastLogin)
	}

	bf.WriteBytes([]byte{0x00, 0x00}) // Unk, might be to do with alliance, 0x00 == no alliance

	for range guildMembers {
		bf.WriteUint32(0x00) // Unk
	}

	doAckBufSucceed(s, pkt.AckHandle, bf.Data())
}

func handleMsgMhfGetGuildManageRight(s *Session, p mhfpacket.MHFPacket) {
	pkt := p.(*mhfpacket.MsgMhfGetGuildManageRight)

	guild, err := GetGuildInfoByCharacterId(s, s.charID)

	if err != nil {
		s.logger.Warn("failed to respond to manage rights message")
		return
	} else if guild == nil {
		bf := byteframe.NewByteFrame()
		bf.WriteUint16(0x00) // Unk
		bf.WriteUint16(0x00) // Member count

		doAckBufSucceed(s, pkt.AckHandle, bf.Data())
		return
	}

	bf := byteframe.NewByteFrame()

	bf.WriteUint16(0x00) // Unk
	bf.WriteUint16(guild.MemberCount)

	members, err := GetGuildMembers(s, guild.ID, false)

	for _, member := range members {
		bf.WriteUint32(member.CharID)
		bf.WriteUint32(0x0)
	}

	doAckBufSucceed(s, pkt.AckHandle, bf.Data())
}

func handleMsgMhfSetGuildManageRight(s *Session, p mhfpacket.MHFPacket) {}

func handleMsgMhfGetUdGuildMapInfo(s *Session, p mhfpacket.MHFPacket) {
	pkt := p.(*mhfpacket.MsgMhfGetUdGuildMapInfo)

	data, _ := hex.DecodeString("00050000013600000137013500000000E2DF000000000204000000640100000001019901350000E2DF0000000001000000044C000000000001FE01FF00000000000000000D0000001036000000000001FC01FD00000000000000000B0000000F0A000000000001FB01FC00000000000000000A0000000E740000000000019B013700000000000000000F00000011620000000000019601FB0000000000000000090000000DDE0000000000013700D400000000000000001000000011F80000000000013201960000000000000000080000000D48000000000000D40070000000000000000011000000128E000000000000CE01320000000000000000070000000CB200000000000070006F00000000000000001200000013240000000000006F006E00000000000000001300000013BA0000000000006E006D00000000000000001400000014500000000000006D0000000000000000000015020000157C0000000000006A00CE0000000000000000060000000C1C00000000000069006A0000000000000000050000000B860000000000006800690000000000000000040000000AF00000000000006700680000000000000000030000000A5A00000000000066006700000000000000000200000009C4000000000001FD01FE01990000000000000C0300000FA00000000000006500660000000000000000010100000000000000000001FF019B00000000000000000E00000010CC0000000000000000000000000000000000030000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000013700000138013200000000E2E0000000000204000000640100000002019701320000E2E00000000001000000044C00000000000193012E00000000000000000E000000319C0000000000013701360000000000000000060000001EDC00000000000136019900000000000000000700000021340000000000012E012D00000000000000000F00000033F40000000000019801FB01970000000000000903000025E4000000000001F9019400000000000000000C0000002CEC0000000000012D00C9000000000000000010000000364C000000000000D401370000000000000000050000001C84000000000000C9006600000000000000001100000036B00000000000007000D40000000000000000040000001A2C000000000001FA01F900000000000000000B0000002A940000000000006F007000000000000000000300000017D40000000000006E006F000000000000000002000000157C0000000000006D006E00000000000000000101000000000000000000006900000000000000000000150200004362000000000001FB01FA00000000000000000A000000283C0000000000006800690000000000000000140000003B6000000000000067006800000000000000001300000039D0000000000001990198000000000000000008000000238C000000000000660067000000000000000012000000390800000000000194019300000000000000000D0000002F44000000000000000000000000000000000003000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000001380000013901FF00000000E2E10000000001040000044C0000000003019B013601FF0000000000000D0300003BC4000000000001FA000000000000000000001502000055F0000000000001F901FA00000000000000001400000051A400000000000199013400000000000000000F00000042040000000000019501F90000000000000000130000004E8400000000000138019B00000000000000000C00000038A400000000000136019900000000000000000E0000003EE4000000000001340133000000000000000010000000452400000000000133013200000000000000001100000048440000000000013201950000000000000000120000004B64000000000000D4013800000000000000000B0000003584000000000000D1006E0000000000000000070000002904000000000000CD006A0000000000000000030000001C840000000000007000D400000000000000000A00000032640000000000006F00700000000000000000090000002F440000000000006E006F0000000000000000080000002C240000000000006C00D100000000000000000600000025E40000000000006B006C00000000000000000500000022C40000000000006A006B0000000000000000040000001FA40000000000006800CD000000000000000002000000196400000000000067006800000000000000000101000000000000000000000000000000000000000000020000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000001390000013A006500000000E2E200000000020400000064010000000500C900650000E2E20000000001000000044C00000000000133019700000000000000000C040000445C0000000004013700D20000000000000000130000005FB4000000000000CA00CB00C9000000000000060300002CEC000000000001FF019B00000000000000001100000057E4000000000001FE01FF00000000000000001000000053FC000000000001FD01FE00000000000000000F0000005014000000000001FA01F90000000000000000010100000000000000000001F901940000000000000000020000001D4C0000000000019B01370000000000000000120000005BCC0000000000019801FD00000000000000000E0000004C2C00000000000197019800000000000000000D0000004844000000000001940193000000000000000003000000213400000000000193012E000000000000000004000000251C0000000000012E00CA0000000000000000050000002904000000000000D2006E000000000000000014000000639C000000000000CF013300000000000000000B0000004074000000000000CB006800000000000000000700000030D40000000000006E000000000000000000001502000075300000000000006A00CF00000000000000000A0000003C8C00000000000069006A00000000000000000900000038A400000000000068006900000000000000000800000034BC0000000000000000000000000000000000030000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000013A0000013701F800000000E2E30000000001040000044C000000000601FD01FC00000000000000000700000034BC000000000001FC01FB00000000000000000800000038A4000000000001FB01FA0000000000000000090000003C8C000000000001FA01F900000000000000000A00000040740000000000019B019A0000000000000000050000002CEC0000000000019A01FD00000000000000000600000030D400000000000195013200000000000000000C000000484400000000000138019B00000000000000000400000029040000000000013300CF00000000000000000E000000501400000000000132013300000000000000000D0000004C2C000000000000D40138000000000000000003000000251C000000000000D300D40000000000000000020000002134000000000000CF006A00000000000000000F00000053FC000000000000CD00CC0000000000000000110000005BCC000000000000CC00CB0000000000000000120000005FB4000000000000CB00CA000000000000000013000000639C000000000000CA00C90000000000000000140000006784000000000000C90000000000000000000015020000FDE80000000000006E00D30000000000000000010100000000000000000001F9019501F80000000000000B030000445C0000000000006A00CD00000000000000001000000057E400000000000000000000000000000000000200000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000009000000010738AD00010001000102000000011A000007D00002006302000000020738AD00020001000102000000021A000007D00002006302000000031A000003E80001006301000000041A000003E80001006301000000050738AD00020001000102000000051A000007D00002006302000000061A000003E800010063010100000136000117000000000000044C019901350000E2DF000000000100000000000000000064013500000000E2DF00000000020400000000000000000000650066000000000000000001010100000000000009C40066006700000000000000000200000000000000000A5A0067006800000000000000000300000000000000000AF00068006900000000000000000400000000000000000B860069006A00000000000000000500000000000000000C1C006A00CE00000000000000000600000000000000000CB200CE013200000000000000000700000000000000000D480132019600000000000000000800000000000000000DDE019601FB00000000000000000900000000000000000E7401FB01FC00000000000000000A00000000000000000F0A01FC01FD00000000000000000B00000000000000000FA001FD01FE01990000000000000C0300000000000000103601FE01FF00000000000000000D000000000000000010CC01FF019B00000000000000000E00000000000000001162019B013700000000000000000F000000000000000011F8013700D40000000000000000100000000000000000128E00D40070000000000000000011000000000000000013240070006F000000000000000012000000000000000013BA006F006E00000000000000001300000000000000001450006E006D0000000000000000140000000000000000157C006D000000000000000000001502000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000")

	doAckBufSucceed(s, pkt.AckHandle, data)
}

func handleMsgMhfGenerateUdGuildMap(s *Session, p mhfpacket.MHFPacket) {}

func handleMsgMhfGetGuildTargetMemberNum(s *Session, p mhfpacket.MHFPacket) {
	pkt := p.(*mhfpacket.MsgMhfGetGuildTargetMemberNum)

	var guild *Guild
	var err error

	if pkt.GuildID == 0x0 {
		guild, err = GetGuildInfoByCharacterId(s, s.charID)
	} else {
		guild, err = GetGuildInfoByID(s, pkt.GuildID)
	}

	if err != nil {
		s.logger.Warn("failed to find guild", zap.Error(err), zap.Uint32("guildID", pkt.GuildID))
		doAckBufSucceed(s, pkt.AckHandle, make([]byte, 4))
		return
	} else if guild == nil {
		doAckBufSucceed(s, pkt.AckHandle, make([]byte, 4))
		return
	}

	bf := byteframe.NewByteFrame()

	bf.WriteUint16(0x0)
	bf.WriteUint16(guild.MemberCount - 1)

	doAckBufSucceed(s, pkt.AckHandle, bf.Data())
}

func handleMsgMhfEnumerateGuildItem(s *Session, p mhfpacket.MHFPacket) {
	pkt := p.(*mhfpacket.MsgMhfEnumerateGuildItem)

	data, _ := hex.DecodeString("000100004cfa00010017000300000000")

	doAckBufSucceed(s, pkt.AckHandle, data)
}

func handleMsgMhfUpdateGuildItem(s *Session, p mhfpacket.MHFPacket) {}

func handleMsgMhfUpdateGuildIcon(s *Session, p mhfpacket.MHFPacket) {
	pkt := p.(*mhfpacket.MsgMhfUpdateGuildIcon)

	guild, err := GetGuildInfoByID(s, pkt.GuildID)

	if err != nil {
		panic(err)
	}

	characterInfo, err := GetCharacterGuildData(s, s.charID)

	if err != nil {
		panic(err)
	}

	if !characterInfo.IsSubLeader() && !characterInfo.IsLeader {
		s.logger.Warn(
			"character without leadership attempting to update guild icon",
			zap.Uint32("guildID", guild.ID),
			zap.Uint32("charID", s.charID),
		)
		doAckSimpleFail(s, pkt.AckHandle, make([]byte, 4))
		return
	}

	icon := &GuildIcon{}

	icon.Parts = make([]GuildIconPart, pkt.PartCount)

	for i, p := range pkt.IconParts {
		icon.Parts[i] = GuildIconPart{
			Index:    p.Index,
			ID:       p.ID,
			Size:     p.Size,
			Rotation: p.Rotation,
			PosX:     p.PosX,
			PosY:     p.PosY,
		}
	}

	guild.Icon = icon

	err = guild.Save(s)

	if err != nil {
		doAckSimpleFail(s, pkt.AckHandle, make([]byte, 4))
		return
	}

	doAckSimpleSucceed(s, pkt.AckHandle, make([]byte, 4))
}
