package game

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"time"
)

// 重连使用客户端已鉴权收尾提交的 auth_msg；数据库仅保存摘要，并绑定角色与设备。
// RE_CONNECTION 必须复用客户端已有 Avatar，不能创建新的 Account。
type ReconnectAccounts interface {
	SaveReconnect(context.Context, []byte, string, []byte, time.Time) error
	Resume(context.Context, []byte, string, []byte, time.Time) (Identity, error)
}

type reconnectRecord struct {
	identity Identity
	device   string
	hash     [32]byte
	expires  time.Time
}

func (a *FixtureAccounts) SaveReconnect(ctx context.Context, oid []byte, device string, hash []byte, expires time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	for account, record := range a.records {
		for _, av := range record.Avatars {
			if string(av.OID) == string(oid) {
				if a.reconnect == nil {
					a.reconnect = map[string]reconnectRecord{}
				}
				var digest [32]byte
				copy(digest[:], hash)
				a.reconnect[string(oid)] = reconnectRecord{Identity{Account: account, Avatars: []Avatar{av}}, device, digest, expires}
				return nil
			}
		}
	}
	return errors.New("角色不存在")
}

func (a *FixtureAccounts) Resume(ctx context.Context, oid []byte, device string, hash []byte, now time.Time) (Identity, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Identity{}, err
	}
	record, ok := a.reconnect[string(oid)]
	if !ok || record.device != device || !now.Before(record.expires) || subtle.ConstantTimeCompare(hash, record.hash[:]) != 1 {
		return Identity{}, errors.New("重连凭证无效或已过期")
	}
	identity := Identity{Account: record.identity.Account, Avatars: append([]Avatar(nil), record.identity.Avatars...)}
	for i, av := range identity.Avatars {
		for _, current := range a.records[identity.Account].Avatars {
			if string(current.OID) == string(av.OID) {
				identity.Avatars[i] = current
				break
			}
		}
	}
	return identity, nil
}

func (c *Connection) SetDeviceID(device string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deviceID = device
}

// 调用者已经持有连接锁，提交成功后才能允许客户端进入重连路径。
func (s *Service) saveReconnect(ctx context.Context, c *Connection, raw json.RawMessage) error {
	var token string
	if json.Unmarshal(raw, &token) != nil || len(token) == 0 || len(token) > 512 {
		return errors.New("重连凭证必须是非空短字符串")
	}
	store, ok := s.Accounts.(ReconnectAccounts)
	if !ok {
		return errors.New("账号存储不支持重连")
	}
	for _, av := range c.identity.Avatars {
		if av.Hostnum == c.hostnum {
			digest := sha256.Sum256([]byte(token))
			return store.SaveReconnect(ctx, av.OID, c.deviceID, digest[:], s.Now().Add(24*time.Hour))
		}
	}
	return errors.New("没有绑定角色")
}

// Resume校验凭据并复用原Avatar。普通未完成战斗丢失连接观察缓存，
// 必须同批下发新UUID、明确清场标记与prepare；PVP保留其独立原生恢复路线。
// 恢复成功即完成会话接管：同角色旧连接移出在线注册表并终止，
// 新连接登记为唯一在线会话；任何一步失败都整体回滚，
// 连接不得停留在半接管状态继续接收角色业务。
func (s *Service) Resume(ctx context.Context, c *Connection, oid []byte, device string, token []byte) (Avatar, []Push, error) {
	c.mu.Lock()
	if c.phase != Connected || len(oid) != 12 || len(token) == 0 || len(token) > 512 {
		c.mu.Unlock()
		return Avatar{}, nil, errors.New("重连状态或参数无效")
	}
	store, ok := s.Accounts.(ReconnectAccounts)
	if !ok {
		c.mu.Unlock()
		return Avatar{}, nil, errors.New("账号存储不支持重连")
	}
	digest := sha256.Sum256(token)
	identity, err := store.Resume(ctx, oid, device, digest[:], s.Now())
	if err != nil {
		c.mu.Unlock()
		return Avatar{}, nil, err
	}
	var av Avatar
	matched := false
	for _, candidate := range identity.Avatars {
		if string(candidate.OID) == string(oid) {
			av, matched = candidate, true
			break
		}
	}
	if !matched {
		c.mu.Unlock()
		return Avatar{}, nil, errors.New("重连角色不属于账号")
	}
	c.identity, c.hostnum, c.phase, c.deviceID = identity, av.Hostnum, Playing, device
	c.refreshLoginSent = true
	// 客户端权威：重连不再补发服务器战斗跳转；已开战会话仅恢复
	// battleStartSent 防止 battle_fighting 重复上行，未完成战斗由
	// 客户端 client_need_recover_battle 请求驱动重开准备阶段。
	if av.Progress.Battle != nil && av.Progress.Battle.Started {
		c.battleStartSent = true
	}
	var replay []Push
	p := av.Progress
	if b := p.Battle; b != nil && !b.Finished && b.NativeSolo == nil && p.SyncPvpMatch == nil && p.Social.HumanRoom == nil && (p.AsyncPvp.Match == nil || p.AsyncPvp.Match.UUID != b.UUID) {
		// 真正TCP重连丢失连接观察缓存与未送达帧，不能续用旧序号。
		// 先走已授权的普通冷恢复事务，新UUID与明确清场标记随connect_reply整批下发。
		replay, err = s.clientNeedRecoverBattle(ctx, c, nil)
		if err != nil {
			// 冷恢复失败整体回滚：连接回到未接管状态，可重新登录或重试。
			c.identity, c.hostnum, c.phase, c.deviceID = Identity{}, 0, Connected, ""
			c.refreshLoginSent = false
			c.battleStartSent = false
			c.mu.Unlock()
			return Avatar{}, nil, err
		}
	}
	// 接管：同角色旧连接先移出注册表，新连接登记为唯一在线会话，
	// 邮箱/在线刷新等共享投递不会再同时写两条连接。
	predecessors := s.takeoverConnection(c, oid)
	s.initializeHumanDelivery(c)
	if err := s.refreshHumanPresence(ctx, c); err != nil {
		log.Printf("真人在线租约登记失败，将由Tick重试：%v", err)
	}
	resumed := c.SelectedAvatarUnsafe()
	c.mu.Unlock()
	// 终止前任连接时不持有新连接锁：旧连接业务收尾可能向新连接投递快照。
	for _, old := range predecessors {
		old.Close()
		old.mu.Lock()
		presence := old.humanPresenceID
		old.humanPresenceID = ""
		old.mu.Unlock()
		s.removeHumanPresenceByID(presence)
	}
	if len(replay) > 0 {
		return resumed, replay, nil
	}
	return av, nil, nil
}
