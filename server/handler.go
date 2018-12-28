package server

import (
	"errors"
	"fmt"
	"github.com/rs/xid"
	"github.com/slince/spike-go/protol"
	"github.com/slince/spike-go/tunnel"
	"net"
)

// 消息处理器接口
type MessageHandler interface {
	// Handle the message
	Handle(message *protol.Protocol) error
}

type Handler struct{
	connection net.Conn
	server *Server
}

// 客户端注册时消息处理器
type AuthHandler struct{
	Handler
}

func (hd *AuthHandler) Handle(message *protol.Protocol) error{
	//验证客户端凭证
	auth, ok := message.Body["auth"]
	if !ok {
		msg := &protol.Protocol{
			Action: "auth_response",
			Headers: map[string]string{"code": "403"},
			Body: map[string]interface{}{"error": "bad request"},
		}
		hd.server.sendMessage(hd.connection, msg)
		return errors.New("bad request")
	}
	err := hd.server.Authentication.Auth(auth.(map[string]interface{}))
	var msg *protol.Protocol
	if err != nil {
		msg = &protol.Protocol{
			Action: "auth_response",
			Headers: map[string]string{"code": "403"},
		}
	} else {
		guid := xid.New().String()
		client := &Client{
			Conn: hd.connection,
			Id:   guid,
		}
		hd.server.Clients[guid] = client

		msg = &protol.Protocol{
			Action: "auth_response",
			Headers: map[string]string{"code": "200"},
			Body: map[string]interface{}{"client": client},
		}
	}
	hd.server.sendMessage(hd.connection, msg)
	return nil
}

// 心跳包处理器
type PingHandler struct{
	Handler
}

func (hd *PingHandler) Handle(message *protol.Protocol) error{
	msg := &protol.Protocol{
		Action: "pong",
	}
	hd.server.sendMessage(hd.connection, msg)
	return nil
}


// 需要验证之后的消息处理器
type RequireAuthHandler struct {
	Handler
	client *Client
}

func (hd *RequireAuthHandler) isAuthenticated(message *protol.Protocol) bool{
	clientId, ok := message.Headers["client-id"]
	if !ok {
		return false
	}
	if client, ok := hd.server.Clients[clientId]; ok{
		hd.client = client
		return true
	}
	return false
}

// 客户端注册隧道时的消息处理器
type RegisterTunnelHandler struct{
	RequireAuthHandler
}

func (hd *RegisterTunnelHandler) Handle(message *protol.Protocol) error{
	if !hd.isAuthenticated(message) {
		return errors.New("the client is not authorized")
	}

	tunnelsInfo, ok := message.Body["tunnels"]
	if !ok {
		return errors.New("missing tunnel info")
	}
	infos := tunnelsInfo.([]interface{})
	var details = make([]map[string]interface{}, len(infos))
	for idx,info := range infos{
		details[idx] = info.(map[string]interface{})
	}

	//创建tunnel
	tunnels := tunnel.NewManyTunnels(details)
	registeredTunnels := make([]tunnel.Tunnel, 0)
	var chunkServers = make([]ChunkServer, 0)
	for _,tn := range tunnels {
		//如果tunnel已经注册则拒绝再次注册
		if hd.server.tunnelIsReg(tn) {
			msg := &protol.Protocol{
				Action: "register_tunnel_response",
				Headers: map[string]string{"code": "1"},
				Body: map[string]interface{}{
					"error": "The tunnel has been registered",
					"tunnel": tn,
				},
			}
			hd.server.sendMessage(hd.connection, msg)
			continue
		}
		//创建对应的chunk server
		chunkServer,err := newChunkServer(tn, hd.server, hd.client)
		if err != nil {
			msg := &protol.Protocol{
				Action: "register_tunnel_response",
				Headers: map[string]string{"code": "2"},
				Body: map[string]interface{}{
					"error": "Error create chunk server.",
					"tunnel": tn,
				},
			}
			hd.server.Logger.Warn("fail to create chunk server for the tunnel", err)
			hd.server.sendMessage(hd.connection, msg)
			continue
		}
		registeredTunnels = append(registeredTunnels, tn)
		chunkServers = append(chunkServers, chunkServer)
	}
	//如果有成功
	if len(registeredTunnels) > 0 {
		//追加入客户端的chunk servers,
		hd.client.ChunkServers = append(hd.client.ChunkServers, chunkServers...)
		for _, chunkServer := range chunkServers {
			hd.server.chunkServerChain <-chunkServer
		}
		//注册成功的客户端
		msg := &protol.Protocol{
			Action: "register_tunnel_response",
			Headers: map[string]string{"code": "200"},
			Body: map[string]interface{}{"tunnels": registeredTunnels},
		}

		hd.server.sendMessage(hd.connection, msg)
		return nil
	} else {
		return errors.New("no tunnel is registered")
	}
}

// 创建chunk server
func newChunkServer(tn tunnel.Tunnel, server *Server, client *Client) (ChunkServer,error){
	var chunkServer ChunkServer
	//生成tunnel的id
	tunnelId := xid.New().String()

	switch tn := tn.(type) {
	case *tunnel.TcpTunnel:
		tn.Id = tunnelId
		chunkServer = &TcpChunkServer{
			Tunnel: tn,
			Client: client,
			Server: server,
			pubConnCollection: make(map[string]*PublicConn, 0),
		}
	case *tunnel.HttpTunnel:
		tn.Id = tunnelId
		tcpChunkServer := TcpChunkServer{
			Tunnel: &tn.TcpTunnel,
			Client: client,
			Server: server,
			pubConnCollection: make(map[string]*PublicConn, 0),
		}
		chunkServer = &HttpChunkServer{
			TcpChunkServer: tcpChunkServer,
		}
	default:
		return nil, fmt.Errorf("bad tunnel")
	}
	return chunkServer,nil
}

// 注册代理消息处理器
type RegisterProxyHandler struct{
	RequireAuthHandler
}

func (hd *RegisterProxyHandler) Handle(message *protol.Protocol) error{
	if !hd.isAuthenticated(message) {
		return errors.New("the client is not authorized")
	}

	tunnelId, ok := message.Headers["tunnel-id"]
	if !ok {
		return fmt.Errorf("missing tunnel id")
	}
	chunkServer := hd.server.findChunkServerByTunId(tunnelId)
	if chunkServer == nil {
		return fmt.Errorf("the chunk server %s is not found", tunnelId)
	}
	publicConnectionId,ok := message.Headers["public-connection-id"]
	if !ok { //错误的注册代理协议
		return fmt.Errorf("missing public id")
	}
	// set proxy connection
	chunkServer.SetProxyConnection(publicConnectionId, hd.connection)
	return nil
}

// 消息处理器创建工厂
type MessageHandlerFactory struct {
	Conn net.Conn
	Server *Server
}

func (factory MessageHandlerFactory) newHandler() Handler{
	return Handler{
		factory.Conn,
		factory.Server,
	}
}

func (factory MessageHandlerFactory) NewAuthHandler() MessageHandler{
	var handler MessageHandler
	handler = &AuthHandler{
		factory.newHandler(),
	}
	return handler
}

func (factory MessageHandlerFactory) NewPingHandler() MessageHandler{
	var handler MessageHandler
	handler = &PingHandler{
		factory.newHandler(),
	}
	return handler
}

func (factory MessageHandlerFactory) NewRegisterTunnelHandler() MessageHandler{
	var handler MessageHandler
	handler = &RegisterTunnelHandler{
		RequireAuthHandler{
			factory.newHandler(),
			nil,
		},
	}
	return handler
}

func (factory MessageHandlerFactory) NewRegisterProxyHandler() MessageHandler{
	var handler MessageHandler
	handler = &RegisterProxyHandler{
		RequireAuthHandler{
			factory.newHandler(),
			nil,
		},
	}
	return handler
}