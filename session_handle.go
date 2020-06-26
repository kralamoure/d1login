package d1login

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/alexedwards/argon2id"
	"github.com/gofrs/uuid"
	"github.com/kralamoure/d1"
	"github.com/kralamoure/d1/filter"
	"github.com/kralamoure/d1/typ"
	"github.com/kralamoure/d1proto/enum"
	"github.com/kralamoure/d1proto/msgcli"
	"github.com/kralamoure/d1proto/msgsvr"
	prototyp "github.com/kralamoure/d1proto/typ"
	"go.uber.org/zap"
)

func (s *session) login(ctx context.Context) error {
	if s.version.Major != 1 || s.version.Minor < 29 {
		s.sendMsg(msgsvr.AccountLoginError{
			Reason: enum.AccountLoginErrorReason.BadVersion,
			Extra:  "^1.29.0",
		})
		versionStr, err := s.version.Serialized()
		if err != nil {
			return err
		}
		s.svr.logger.Debug("wrong version",
			zap.String("client_address", s.conn.RemoteAddr().String()),
			zap.String("version", versionStr),
		)
		return errEndOfService
	}

	if s.credential.CryptoMethod != 1 {
		s.svr.logger.Debug("unhandled crypto method",
			zap.String("client_address", s.conn.RemoteAddr().String()),
			zap.Int("crypto_method", s.credential.CryptoMethod),
		)
		return errEndOfService
	}

	password, err := decryptedPassword(s.credential.Hash, s.salt)
	if err != nil {
		s.svr.logger.Debug("could not decrypt password",
			zap.String("client_address", s.conn.RemoteAddr().String()),
			zap.Error(err),
		)
		return errEndOfService
	}

	account, err := s.svr.svc.Account(ctx, filter.AccountNameEQ(typ.AccountName(s.credential.Username)))
	if err != nil {
		s.sendMsg(msgsvr.AccountLoginError{
			Reason: enum.AccountLoginErrorReason.AccessDenied,
		})
		return err
	}

	user, err := s.svr.svc.User(ctx, filter.UserIdEQ(account.UserId))
	if err != nil {
		return err
	}

	match, err := argon2id.ComparePasswordAndHash(password, string(user.Hash))
	if err != nil {
		return err
	}

	if !match {
		s.sendMsg(msgsvr.AccountLoginError{
			Reason: enum.AccountLoginErrorReason.AccessDenied,
		})
		s.svr.logger.Debug("wrong password",
			zap.String("client_address", s.conn.RemoteAddr().String()),
		)
		return errEndOfService
	}

	err = s.controlAccount(account.Id)
	if err != nil {
		s.sendMsg(msgsvr.AccountLoginError{
			Reason: enum.AccountLoginErrorReason.AlreadyLogged,
		})
		s.svr.logger.Debug("could not control account",
			zap.String("client_address", s.conn.RemoteAddr().String()),
			zap.Error(err),
		)
		return errEndOfService
	}

	s.sendMsg(msgsvr.AccountPseudo{Value: string(user.Nickname)})
	s.sendMsg(msgsvr.AccountCommunity{Id: int(user.Community)})
	s.sendMsg(msgsvr.AccountSecretQuestion{Value: user.Question})

	hosts := msgsvr.AccountHosts{}
	err = hosts.Deserialize(s.svr.hosts.Load())
	if err != nil {
		return err
	}
	s.sendMsg(hosts)

	s.sendMsg(msgsvr.AccountLoginSuccess{Authorized: account.Admin})

	s.status.Store(statusIdle)
	return nil
}

func (s *session) controlAccount(accountId int) error {
	s.svr.mu.Lock()
	defer s.svr.mu.Unlock()
	for sess := range s.svr.sessions {
		if sess.accountId == accountId {
			sess.conn.Close()
			return errors.New("already logged in")
		}
	}
	s.accountId = accountId
	return nil
}

func (s *session) handleAccountVersion(m msgcli.AccountVersion) error {
	s.version = m
	s.status.Store(statusExpectingCredential)
	return nil
}

func (s *session) handleAccountCredential(m msgcli.AccountCredential) error {
	s.credential = m
	s.status.Store(statusExpectingQueuePosition)
	return nil
}

func (s *session) handleAccountQueuePosition(ctx context.Context) error {
	s.sendMsg(msgsvr.AccountNewQueue{
		Position:    1,
		TotalAbo:    0,
		TotalNonAbo: 1,
		Subscriber:  false,
		QueueId:     0,
	})

	if s.status.Load() == statusExpectingQueuePosition {
		return s.login(ctx)
	}

	return nil
}

func (s *session) handleAccountSearchForFriend(ctx context.Context, m msgcli.AccountSearchForFriend) error {
	user, err := s.svr.svc.User(ctx, filter.UserNicknameEQ(typ.Nickname(m.Pseudo)))
	if err != nil {
		if errors.Is(err, d1.ErrResourceNotFound) {
			s.sendMsg(msgsvr.AccountFriendServerList{})
			return nil
		} else {
			return err
		}
	}

	accounts, err := s.svr.svc.Accounts(ctx, filter.AccountUserIdEQ(user.Id))
	if err != nil {
		return err
	}

	serverIdQty := make(map[int]int)

	for _, account := range accounts {
		characters, err := s.svr.svc.Characters(ctx, filter.CharacterAccountIdEQ(account.Id))
		if err != nil {
			return err
		}
		for _, character := range characters {
			serverIdQty[character.GameServerId]++
		}
	}

	var serverCharacters []prototyp.AccountServersListServerCharacters

	for serverId, qty := range serverIdQty {
		serverCharacters = append(serverCharacters, prototyp.AccountServersListServerCharacters{
			Id:  serverId,
			Qty: qty,
		})
	}
	sort.Slice(serverCharacters, func(i, j int) bool { return serverCharacters[i].Id < serverCharacters[j].Id })

	s.sendMsg(msgsvr.AccountFriendServerList{ServersCharacters: serverCharacters})

	return nil
}

func (s *session) handleAccountGetServersList(ctx context.Context) error {
	account, err := s.svr.svc.Account(ctx, filter.AccountIdEQ(s.accountId))
	if err != nil {
		return err
	}

	serverIdQty := make(map[int]int)

	characters, err := s.svr.svc.Characters(ctx, filter.CharacterAccountIdEQ(s.accountId))
	if err != nil {
		return err
	}
	for _, character := range characters {
		serverIdQty[character.GameServerId]++
	}

	var serverCharacters []prototyp.AccountServersListServerCharacters

	for serverId, qty := range serverIdQty {
		serverCharacters = append(serverCharacters, prototyp.AccountServersListServerCharacters{
			Id:  serverId,
			Qty: qty,
		})
	}
	sort.Slice(serverCharacters, func(i, j int) bool { return serverCharacters[i].Id < serverCharacters[j].Id })

	s.sendMsg(msgsvr.AccountServersListSuccess{
		Subscription:      account.SubscribedUntil,
		ServersCharacters: serverCharacters,
	})

	return nil
}

func (s *session) handleAccountSetServer(ctx context.Context, m msgcli.AccountSetServer) error {
	gameServer, err := s.svr.svc.GameServer(ctx, filter.GameServerIdEQ(m.Id))
	if err != nil {
		return err
	}

	ticketId, err := uuid.NewV4()
	if err != nil {
		return err
	}

	_, err = s.svr.svc.CreateTicket(ctx, d1.Ticket{
		Id:           ticketId.String(),
		AccountId:    s.accountId,
		GameServerId: m.Id,
		Created:      time.Now().UTC(),
	})
	if err != nil {
		return err
	}

	s.sendMsg(msgsvr.AccountSelectServerPlainSuccess{
		Host:   gameServer.Host,
		Port:   gameServer.Port,
		Ticket: ticketId.String(),
	})

	return errEndOfService
}
