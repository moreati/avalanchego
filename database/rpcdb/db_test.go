// (c) 2019-2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package rpcdb

import (
	"log"
	"net"
	"testing"

	"golang.org/x/net/context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"

	"github.com/ava-labs/gecko/database"
	"github.com/ava-labs/gecko/database/memdb"
	"github.com/ava-labs/gecko/database/rpcdb/proto"
)

const (
	bufSize = 1 << 20
)

func TestInterface(t *testing.T) {
	for _, test := range database.Tests {
		listener := bufconn.Listen(bufSize)
		server := grpc.NewServer()
		proto.RegisterDatabaseServer(server, NewServer(memdb.New()))
		go func() {
			if err := server.Serve(listener); err != nil {
				log.Fatalf("Server exited with error: %v", err)
			}
		}()

		dialer := grpc.WithContextDialer(
			func(context.Context, string) (net.Conn, error) {
				return listener.Dial()
			})

		ctx := context.Background()
		conn, err := grpc.DialContext(ctx, "", dialer, grpc.WithInsecure())
		if err != nil {
			t.Fatalf("Failed to dial: %s", err)
		}

		db := NewClient(proto.NewDatabaseClient(conn))
		test(t, db)
		conn.Close()
	}
}
