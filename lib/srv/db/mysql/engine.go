/*
Copyright 2021 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mysql

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/srv/db/common"
	"github.com/gravitational/teleport/lib/srv/db/common/role"
	"github.com/gravitational/teleport/lib/srv/db/mysql/protocol"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/siddontang/go-mysql/client"
	"github.com/siddontang/go-mysql/packet"
	"github.com/siddontang/go-mysql/server"

	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"github.com/sirupsen/logrus"
)

// Engine implements the MySQL database service that accepts client
// connections coming over reverse tunnel from the proxy and proxies
// them between the proxy and the MySQL database instance.
//
// Implements common.Engine.
type Engine struct {
	// Auth handles database access authentication.
	Auth common.Auth
	// Audit emits database access audit events.
	Audit common.Audit
	// AuthClient is the cluster auth server client.
	AuthClient *auth.Client
	// Context is the database server close context.
	Context context.Context
	// Clock is the clock interface.
	Clock clockwork.Clock
	// Log is used for logging.
	Log logrus.FieldLogger
	// proxyConn is a client connection.
	proxyConn server.Conn
}

// InitializeConnection initializes the engine with client connection.
func (e *Engine) InitializeConnection(clientConn net.Conn, _ *common.Session) error {
	// Make server conn to get access to protocol's WriteOK/WriteError methods.
	e.proxyConn = server.Conn{Conn: packet.NewConn(clientConn)}
	return nil
}

// SendError sends an error to connected client in the MySQL understandable format.
func (e *Engine) SendError(err error) {
	if writeErr := e.proxyConn.WriteError(err); writeErr != nil {
		e.Log.WithError(writeErr).Debugf("Failed to send error %q to MySQL client.", err)
	}
}

// HandleConnection processes the connection from MySQL proxy coming
// over reverse tunnel.
//
// It handles all necessary startup actions, authorization and acts as a
// middleman between the proxy and the database intercepting and interpreting
// all messages i.e. doing protocol parsing.
func (e *Engine) HandleConnection(ctx context.Context, sessionCtx *common.Session) error {
	// Perform authorization checks.
	err := e.checkAccess(ctx, sessionCtx)
	if err != nil {
		return trace.Wrap(err)
	}
	// Establish connection to the MySQL server.
	serverConn, err := e.connect(ctx, sessionCtx)
	if err != nil {
		if trace.IsLimitExceeded(err) {
			return trace.LimitExceeded("could not connect to the database, please try again later")
		}
		return trace.Wrap(err)
	}
	defer func() {
		err := serverConn.Close()
		if err != nil {
			e.Log.WithError(err).Error("Failed to close connection to MySQL server.")
		}
	}()
	// Send back OK packet to indicate auth/connect success. At this point
	// the original client should consider the connection phase completed.
	err = e.proxyConn.WriteOK(nil)
	if err != nil {
		return trace.Wrap(err)
	}
	e.Audit.OnSessionStart(e.Context, sessionCtx, nil)
	defer e.Audit.OnSessionEnd(e.Context, sessionCtx)
	// Copy between the connections.
	clientErrCh := make(chan error, 1)
	serverErrCh := make(chan error, 1)
	go e.receiveFromClient(e.proxyConn.Conn, serverConn, clientErrCh, sessionCtx)
	go e.receiveFromServer(serverConn, e.proxyConn.Conn, serverErrCh)
	select {
	case err := <-clientErrCh:
		e.Log.WithError(err).Debug("Client done.")
	case err := <-serverErrCh:
		e.Log.WithError(err).Debug("Server done.")
	case <-ctx.Done():
		e.Log.Debug("Context canceled.")
	}
	return nil
}

// checkAccess does authorization check for MySQL connection about to be established.
func (e *Engine) checkAccess(ctx context.Context, sessionCtx *common.Session) error {
	ap, err := e.Auth.GetAuthPreference(ctx)
	if err != nil {
		return trace.Wrap(err)
	}
	mfaParams := services.AccessMFAParams{
		Verified:       sessionCtx.Identity.MFAVerified != "",
		AlwaysRequired: ap.GetRequireSessionMFA(),
	}
	dbRoleMatchers := role.DatabaseRoleMatchers(
		defaults.ProtocolMySQL,
		sessionCtx.DatabaseUser,
		sessionCtx.DatabaseName,
	)
	err = sessionCtx.Checker.CheckAccess(
		sessionCtx.Database,
		mfaParams,
		dbRoleMatchers...,
	)
	if err != nil {
		e.Audit.OnSessionStart(e.Context, sessionCtx, err)
		return trace.Wrap(err)
	}
	return nil
}

// connect establishes connection to MySQL database.
func (e *Engine) connect(ctx context.Context, sessionCtx *common.Session) (*client.Conn, error) {
	tlsConfig, err := e.Auth.GetTLSConfig(ctx, sessionCtx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	// clientOpt sets tlsConfig in the mysql client.
	// this is overridden for Cloud SQL secure connections.
	clientOpt := func(conn *client.Conn) {
		conn.SetTLSConfig(tlsConfig)
	}
	user := sessionCtx.DatabaseUser
	var password string
	switch {
	case sessionCtx.Database.IsRDS():
		password, err = e.Auth.GetRDSAuthToken(sessionCtx)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	case sessionCtx.Database.IsCloudSQL():
		// For Cloud SQL MySQL there is no IAM auth so we use one-time passwords
		// by resetting the database user password for each connection. Thus,
		// acquire a lock to make sure all connection attempts to the same
		// database and user are serialized.
		retryCtx, cancel := context.WithTimeout(ctx, defaults.DatabaseConnectTimeout)
		defer cancel()
		lease, err := services.AcquireSemaphoreWithRetry(retryCtx, e.makeAcquireSemaphoreConfig(sessionCtx))
		if err != nil {
			return nil, trace.Wrap(err)
		}
		// Only release the semaphore after the connection has been established
		// below. If the semaphore fails to release for some reason, it will
		// expire in a minute on its own.
		defer func() {
			err := e.AuthClient.CancelSemaphoreLease(ctx, *lease)
			if err != nil {
				e.Log.WithError(err).Errorf("Failed to cancel lease: %v.", lease)
			}
		}()
		password, err = e.Auth.GetCloudSQLPassword(ctx, sessionCtx)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		// workaround issue generating ephemeral certs for secure connections
		// by creating a TLS connection to the cloud proxy port (3307) and
		// overridding mysql client's connection. mysql on 3306 does not trust
		// the ephemeral cert's CA but cloud proxy does.
		if sessionCtx.Database.GetTLS().Mode != types.DatabaseTLSMode_INSECURE {
			uri := sessionCtx.Database.GetURI()
			host, port, err := net.SplitHostPort(uri)
			if err == nil && port == "3306" {
				uri = net.JoinHostPort(host, "3307")
				e.Log.Debug("Overrided URI port from 3306 to 3307")
			}
			tlsconn, err := tls.Dial("tcp", uri, tlsConfig)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			clientOpt = func(conn *client.Conn) {
				conn.Conn.Close() // close connection created by mysql client
				conn.Conn = packet.NewTLSConn(tlsconn)
			}
		}
	case sessionCtx.Database.IsAzure():
		password, err = e.Auth.GetAzureAccessToken(ctx, sessionCtx)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		// Azure requires database login to be <user>@<server-name> e.g.
		// alice@mysql-server-name.
		user = fmt.Sprintf("%v@%v", user, sessionCtx.Database.GetAzure().Name)
	}

	// TODO(r0mant): Set CLIENT_INTERACTIVE flag on the client?
	conn, err := client.Connect(sessionCtx.Database.GetURI(),
		user,
		password,
		sessionCtx.DatabaseName,
		clientOpt)
	if err != nil {
		if trace.IsAccessDenied(common.ConvertError(err)) && sessionCtx.Database.IsRDS() {
			return nil, trace.AccessDenied(`Could not connect to database:

  %v

Make sure that IAM auth is enabled for MySQL user %q and Teleport database
agent's IAM policy has "rds-connect" permissions (note that IAM changes may
take a few minutes to propagate):

%v
`, common.ConvertError(err), sessionCtx.DatabaseUser, sessionCtx.Database.GetIAMPolicy())
		}
		return nil, trace.Wrap(err)
	}
	return conn, nil
}

// receiveFromClient relays protocol messages received from MySQL client
// to MySQL database.
func (e *Engine) receiveFromClient(clientConn, serverConn net.Conn, clientErrCh chan<- error, sessionCtx *common.Session) {
	log := e.Log.WithFields(logrus.Fields{
		"from":   "client",
		"client": clientConn.RemoteAddr(),
		"server": serverConn.RemoteAddr(),
	})
	defer func() {
		log.Debug("Stop receiving from client.")
		close(clientErrCh)
	}()
	for {
		packet, err := protocol.ParsePacket(clientConn)
		if err != nil {
			if utils.IsOKNetworkError(err) {
				log.Debug("Client connection closed.")
				return
			}
			log.WithError(err).Error("Failed to read client packet.")
			clientErrCh <- err
			return
		}
		switch pkt := packet.(type) {
		case *protocol.Query:
			e.Audit.OnQuery(e.Context, sessionCtx, common.Query{Query: pkt.Query()})
		case *protocol.ChangeUser:
			// MySQL protocol includes COM_CHANGE_USER command that allows to
			// re-authenticate connection as a different user. It is not
			// supported by mysql shell and most drivers but some drivers do
			// provide it.
			//
			// We do not want to allow changing the connection user and instead
			// force users to go through normal reconnect flow so log the
			// attempt and close the client connection.
			log.Warnf("Rejecting attempt to change user to %q for session %v.", pkt.User(), sessionCtx)
			return
		case *protocol.Quit:
			return
		}
		_, err = protocol.WritePacket(packet.Bytes(), serverConn)
		if err != nil {
			log.WithError(err).Error("Failed to write server packet.")
			clientErrCh <- err
			return
		}
	}
}

// receiveFromServer relays protocol messages received from MySQL database
// to MySQL client.
func (e *Engine) receiveFromServer(serverConn, clientConn net.Conn, serverErrCh chan<- error) {
	log := e.Log.WithFields(logrus.Fields{
		"from":   "server",
		"client": clientConn.RemoteAddr(),
		"server": serverConn.RemoteAddr(),
	})
	defer func() {
		log.Debug("Stop receiving from server.")
		close(serverErrCh)
	}()
	for {
		packet, _, err := protocol.ReadPacket(serverConn)
		if err != nil {
			if utils.IsOKNetworkError(err) {
				log.Debug("Server connection closed.")
				return
			}
			log.WithError(err).Error("Failed to read server packet.")
			serverErrCh <- err
			return
		}
		_, err = protocol.WritePacket(packet, clientConn)
		if err != nil {
			log.WithError(err).Error("Failed to write client packet.")
			serverErrCh <- err
			return
		}
	}
}

// makeAcquireSemaphoreConfig builds parameters for acquiring a semaphore
// for connecting to a MySQL Cloud SQL instance for this session.
func (e *Engine) makeAcquireSemaphoreConfig(sessionCtx *common.Session) services.AcquireSemaphoreWithRetryConfig {
	return services.AcquireSemaphoreWithRetryConfig{
		Service: e.AuthClient,
		// The semaphore will serialize connections to the database as specific
		// user. If we fail to release the lock for some reason, it will expire
		// in a minute anyway.
		Request: types.AcquireSemaphoreRequest{
			SemaphoreKind: "gcp-mysql-token",
			SemaphoreName: fmt.Sprintf("%v-%v", sessionCtx.Database.GetName(), sessionCtx.DatabaseUser),
			MaxLeases:     1,
			Expires:       e.Clock.Now().Add(time.Minute),
		},
		// If multiple connections are being established simultaneously to the
		// same database as the same user, retry for a few seconds.
		Retry: utils.LinearConfig{
			Step:  time.Second,
			Max:   time.Second,
			Clock: e.Clock,
		},
	}
}
