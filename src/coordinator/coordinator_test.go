package coordinator

import (
	. "common"
	"configuration"
	"datastore"
	"encoding/json"
	"fmt"
	"io/ioutil"
	. "launchpad.net/gocheck"
	"net"
	"net/http"
	"os"
	"protocol"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Hook up gocheck into the gotest runner.
func Test(t *testing.T) {
	TestingT(t)
}

type CoordinatorSuite struct{}

var _ = Suite(&CoordinatorSuite{})

func init() {
	runtime.GOMAXPROCS(runtime.NumCPU() * 2)
}

const (
	SERVER_STARTUP_TIME = time.Second * 2
	REPLICATION_LAG     = time.Millisecond * 500
)

type DatastoreMock struct {
	datastore.Datastore
	Series          *protocol.Series
	DroppedDatabase string
}

func (self *DatastoreMock) WriteSeriesData(database string, series *protocol.Series) error {
	self.Series = series
	return nil
}

func (self *DatastoreMock) DropDatabase(database string) error {
	self.DroppedDatabase = database
	return nil
}

func (self *DatastoreMock) LogRequestAndAssignSequenceNumber(request *protocol.Request, replicationFactor *uint8, ownerServerId *uint32) error {
	id := uint64(1)
	request.SequenceNumber = &id
	return nil
}

func (self *DatastoreMock) AtomicIncrement(name string, val int) (uint64, error) {
	return uint64(val), nil
}

func (self *DatastoreMock) ReplayRequestsFromSequenceNumber(*uint32, *uint32, *uint32, *uint8, *uint64, func(*[]byte) error) error {
	return nil
}

func stringToSeries(seriesString string, c *C) *protocol.Series {
	series := &protocol.Series{}
	err := json.Unmarshal([]byte(seriesString), &series)
	c.Assert(err, IsNil)
	return series
}

func startAndVerifyCluster(count int, c *C) []*RaftServer {
	firstPort := 0
	servers := make([]*RaftServer, count, count)
	for i := 0; i < count; i++ {
		l, err := net.Listen("tcp4", ":0")
		c.Assert(err, IsNil)
		servers[i] = newConfigAndServer(c)

		if firstPort == 0 {
			firstPort = l.Addr().(*net.TCPAddr).Port
		} else {
			servers[i].config.SeedServers = []string{fmt.Sprintf("http://localhost:%d", firstPort)}
		}

		servers[i].Serve(l)

		time.Sleep(SERVER_STARTUP_TIME)
		// verify that the server is up
		getConfig(servers[i].port, c)
	}
	return servers
}

func cleanWithoutDeleting(servers ...*RaftServer) {
	for _, server := range servers {
		server.Close()
	}
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
}

func clean(servers ...*RaftServer) {
	cleanWithoutDeleting(servers...)
	for _, server := range servers {
		os.RemoveAll(server.path)
	}
}

func newConfigAndServer(c *C) *RaftServer {
	path, err := ioutil.TempDir(os.TempDir(), "influxdb")
	c.Assert(err, IsNil)
	config := NewClusterConfiguration(&configuration.Configuration{})
	setupConfig := &configuration.Configuration{Hostname: "localhost", RaftDir: path, RaftServerPort: 0}
	server := NewRaftServer(setupConfig, config)
	return server
}

func getConfig(port int, c *C) string {
	host := fmt.Sprintf("localhost:%d", port)
	resp, err := http.Get("http://" + host + "/cluster_config")
	c.Assert(err, Equals, nil)
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	c.Assert(err, Equals, nil)
	return string(body)
}

func assertConfigContains(port int, contains string, isContained bool, c *C) {
	body := getConfig(port, c)
	c.Assert(strings.Contains(string(body), contains), Equals, isContained)
}

func (self *CoordinatorSuite) TestCanCreateCoordinatorWithNoSeed(c *C) {
	server := startAndVerifyCluster(1, c)[0]
	defer clean(server)
}

func (self *CoordinatorSuite) TestCanRecover(c *C) {
	server := startAndVerifyCluster(1, c)[0]
	defer clean(server)

	path, port := server.path, server.port

	server.CreateDatabase("db1", uint8(1))
	assertConfigContains(server.port, "db1", true, c)
	cleanWithoutDeleting(server)
	time.Sleep(SERVER_STARTUP_TIME)
	server = newConfigAndServer(c)
	// reset the path and port to the previous server and remove the
	// path that was created by newConfigAndServer
	os.RemoveAll(server.path)
	server.path = path
	server.port = port
	defer clean(server)
	server.ListenAndServe()
	time.Sleep(time.Second)
	assertConfigContains(server.port, "db1", true, c)
}

func (self *CoordinatorSuite) TestCanSnapshot(c *C) {
	server := startAndVerifyCluster(1, c)[0]
	// defer clean(server)

	path, port, name := server.path, server.port, server.name

	for i := 0; i < 1000; i++ {
		dbname := fmt.Sprintf("db%d", i)
		server.CreateDatabase(dbname, uint8(1))
		assertConfigContains(server.port, dbname, true, c)
	}
	size, err := GetFileSize(server.raftServer.LogPath())
	c.Assert(err, IsNil)
	server.ForceLogCompaction()
	newSize, err := GetFileSize(server.raftServer.LogPath())
	c.Assert(err, IsNil)
	c.Assert(newSize < size, Equals, true)
	fmt.Printf("size of %s shrinked from %d to %d\n", server.raftServer.LogPath(), size, newSize)
	cleanWithoutDeleting(server)
	time.Sleep(SERVER_STARTUP_TIME)
	server = newConfigAndServer(c)
	// reset the path and port to the previous server and remove the
	// path that was created by newConfigAndServer
	os.RemoveAll(server.path)
	server.path = path
	server.port = port
	server.name = name
	// defer clean(server)
	err = server.ListenAndServe()
	c.Assert(err, IsNil)
	time.Sleep(SERVER_STARTUP_TIME)
	for i := 0; i < 1000; i++ {
		dbname := fmt.Sprintf("db%d", i)
		assertConfigContains(server.port, dbname, true, c)
	}

	// make another server join the cluster
	server2 := newConfigAndServer(c)
	defer clean(server2)
	l, err := net.Listen("tcp4", ":0")
	c.Assert(err, IsNil)
	server2.config.SeedServers = []string{fmt.Sprintf("http://localhost:%d", server.port)}
	server2.Serve(l)
	time.Sleep(SERVER_STARTUP_TIME)
	for i := 0; i < 1000; i++ {
		dbname := fmt.Sprintf("db%d", i)
		assertConfigContains(server2.port, dbname, true, c)
	}
}

func (self *CoordinatorSuite) TestCanCreateCoordinatorsAndReplicate(c *C) {
	servers := startAndVerifyCluster(2, c)
	defer clean(servers...)

	err := servers[0].CreateDatabase("db2", uint8(1))
	c.Assert(err, IsNil)
	time.Sleep(REPLICATION_LAG)
	assertConfigContains(servers[0].port, "db2", true, c)
	assertConfigContains(servers[1].port, "db2", true, c)
}

func (self *CoordinatorSuite) TestDoWriteOperationsFromNonLeaderServer(c *C) {
	servers := startAndVerifyCluster(2, c)

	err := servers[1].CreateDatabase("db3", uint8(1))
	c.Assert(err, Equals, nil)
	time.Sleep(REPLICATION_LAG)
	assertConfigContains(servers[0].port, "db3", true, c)
	assertConfigContains(servers[1].port, "db3", true, c)
}

func (self *CoordinatorSuite) TestNewServerJoiningClusterWillPickUpData(c *C) {
	server := startAndVerifyCluster(1, c)[0]
	defer clean(server)
	server.CreateDatabase("db4", uint8(1))
	assertConfigContains(server.port, "db4", true, c)

	server2 := newConfigAndServer(c)
	defer clean(server2)
	l, err := net.Listen("tcp4", ":0")
	c.Assert(err, IsNil)
	server2.config.SeedServers = []string{fmt.Sprintf("http://localhost:%d", server.port)}
	server2.Serve(l)
	time.Sleep(SERVER_STARTUP_TIME)
	assertConfigContains(server2.port, "db4", true, c)
}

func (self *CoordinatorSuite) TestCanElectNewLeaderAndRecover(c *C) {
	servers := startAndVerifyCluster(3, c)
	defer clean(servers...)

	err := servers[0].CreateDatabase("db5", uint8(1))
	c.Assert(err, Equals, nil)
	time.Sleep(REPLICATION_LAG)
	assertConfigContains(servers[0].port, "db5", true, c)
	assertConfigContains(servers[1].port, "db5", true, c)
	assertConfigContains(servers[2].port, "db5", true, c)

	leader, _ := servers[1].leaderConnectString()
	c.Assert(leader, Equals, fmt.Sprintf("http://localhost:%d", servers[0].port))

	// kill the leader
	clean(servers[0])

	// make sure an election will start
	time.Sleep(3 * time.Second)
	leader, _ = servers[1].leaderConnectString()
	c.Assert(leader, Not(Equals), fmt.Sprintf("http://localhost:%d", servers[0].port))
	err = servers[1].CreateDatabase("db6", uint8(1))
	c.Assert(err, Equals, nil)
	time.Sleep(REPLICATION_LAG)
	assertConfigContains(servers[1].port, "db6", true, c)
	assertConfigContains(servers[2].port, "db6", true, c)
}

func (self *UserSuite) BenchmarkHashing(c *C) {
	for i := 0; i < c.N; i++ {
		pwd := fmt.Sprintf("password%d", i)
		_, err := hashPassword(pwd)
		c.Assert(err, IsNil)
	}
}

func (self *CoordinatorSuite) TestAutomaticDbCreations(c *C) {
	servers := startAndVerifyCluster(3, c)
	defer clean(servers...)

	coordinator := NewCoordinatorImpl(&DatastoreMock{}, servers[0], servers[0].clusterConfig)

	time.Sleep(REPLICATION_LAG)

	// Root user is created
	var root User
	var err error
	// we should have the root user
	root, err = coordinator.AuthenticateClusterAdmin("root", "root")
	c.Assert(err, IsNil)
	c.Assert(root.IsClusterAdmin(), Equals, true)

	// can create db users
	c.Assert(coordinator.CreateDbUser(root, "db1", "db_user"), IsNil)
	c.Assert(coordinator.ChangeDbUserPassword(root, "db1", "db_user", "pass"), Equals, nil)

	// the db should be in the index now
	for _, server := range servers {
		coordinator := NewCoordinatorImpl(&DatastoreMock{}, server, server.clusterConfig)
		dbs, err := coordinator.ListDatabases(root)
		c.Assert(err, IsNil)
		c.Assert(dbs, DeepEquals, []*Database{&Database{"db1", 1}})
	}

	// if the db is dropped it should remove the users as well
	c.Assert(servers[0].DropDatabase("db1"), IsNil)
	_, err = coordinator.AuthenticateDbUser("db1", "db_user", "pass")
	c.Assert(err, ErrorMatches, ".*Invalid.*")
}

func (self *CoordinatorSuite) TestAdminOperations(c *C) {
	servers := startAndVerifyCluster(3, c)
	defer clean(servers...)

	coordinator := NewCoordinatorImpl(nil, servers[0], servers[0].clusterConfig)

	time.Sleep(REPLICATION_LAG)

	// Root user is created
	var root User
	var err error
	// we should have the root user
	root, err = coordinator.AuthenticateClusterAdmin("root", "root")
	c.Assert(err, IsNil)
	c.Assert(root.IsClusterAdmin(), Equals, true)
	c.Assert(root.HasWriteAccess("foobar"), Equals, true)
	c.Assert(root.HasReadAccess("foobar"), Equals, true)

	// Can change it's own password
	c.Assert(coordinator.ChangeClusterAdminPassword(root, "root", "password"), Equals, nil)
	root, err = coordinator.AuthenticateClusterAdmin("root", "password")
	c.Assert(err, IsNil)
	c.Assert(root.IsClusterAdmin(), Equals, true)

	// Can create other cluster admin
	c.Assert(coordinator.CreateClusterAdminUser(root, "another_cluster_admin"), IsNil)
	c.Assert(coordinator.CreateClusterAdminUser(root, ""), NotNil)
	c.Assert(coordinator.ChangeClusterAdminPassword(root, "another_cluster_admin", "pass"), IsNil)
	u, err := coordinator.AuthenticateClusterAdmin("another_cluster_admin", "pass")
	c.Assert(err, IsNil)
	c.Assert(u.IsClusterAdmin(), Equals, true)

	// can get other cluster admin
	admins, err := coordinator.ListClusterAdmins(root)
	c.Assert(err, IsNil)
	sort.Strings(admins)
	c.Assert(admins, DeepEquals, []string{"another_cluster_admin", "root"})

	// can create db users
	c.Assert(coordinator.CreateDbUser(root, "db1", "db_user"), IsNil)
	c.Assert(coordinator.CreateDbUser(root, "db1", ""), NotNil)
	c.Assert(coordinator.ChangeDbUserPassword(root, "db1", "db_user", "db_pass"), IsNil)
	u, err = coordinator.AuthenticateDbUser("db1", "db_user", "db_pass")
	c.Assert(err, IsNil)
	c.Assert(u.IsClusterAdmin(), Equals, false)
	c.Assert(u.IsDbAdmin("db1"), Equals, false)

	// can make db users db admins
	c.Assert(coordinator.SetDbAdmin(root, "db1", "db_user", true), IsNil)
	u, err = coordinator.AuthenticateDbUser("db1", "db_user", "db_pass")
	c.Assert(err, IsNil)
	c.Assert(u.IsDbAdmin("db1"), Equals, true)

	// can list db users
	dbUsers, err := coordinator.ListDbUsers(root, "db1")
	c.Assert(err, IsNil)
	c.Assert(dbUsers, DeepEquals, []string{"db_user"})

	// can delete cluster admins and db users
	c.Assert(coordinator.DeleteDbUser(root, "db1", "db_user"), IsNil)
	c.Assert(coordinator.DeleteClusterAdminUser(root, "another_cluster_admin"), IsNil)
}

func (self *CoordinatorSuite) TestContinuousQueryOperations(c *C) {
	servers := startAndVerifyCluster(3, c)
	defer clean(servers...)

	coordinator := NewCoordinatorImpl(nil, servers[0], servers[0].clusterConfig)

	time.Sleep(REPLICATION_LAG)

	// create users
	root, _ := coordinator.AuthenticateClusterAdmin("root", "root")

	coordinator.CreateDbUser(root, "db1", "db_admin")
	coordinator.ChangeDbUserPassword(root, "db1", "db_admin", "db_pass")
	coordinator.SetDbAdmin(root, "db1", "db_admin", true)
	dbAdmin, _ := coordinator.AuthenticateDbUser("db1", "db_admin", "db_pass")

	coordinator.CreateDbUser(root, "db1", "db_user")
	coordinator.ChangeDbUserPassword(root, "db1", "db_user", "db_pass")
	dbUser, _ := coordinator.AuthenticateDbUser("db1", "db_user", "db_pass")

	allowedUsers := []*User{&root, &dbAdmin}
	disallowedUsers := []*User{&dbUser}

	// cluster admins and db admins should be able to do everything
	for _, user := range allowedUsers {
		results, err := coordinator.ListContinuousQueries(*user, "db1")
		c.Assert(err, IsNil)
		c.Assert(results[0].Points, HasLen, 0)

		c.Assert(coordinator.CreateContinuousQuery(*user, "db1", "select * from foo into bar;"), IsNil)
		time.Sleep(REPLICATION_LAG)
		results, err = coordinator.ListContinuousQueries(*user, "db1")
		c.Assert(err, IsNil)
		c.Assert(results[0].Points, HasLen, 1)
		c.Assert(*results[0].Points[0].Values[0].Int64Value, Equals, int64(1))
		c.Assert(*results[0].Points[0].Values[1].StringValue, Equals, "select * from foo into bar;")

		c.Assert(coordinator.DeleteContinuousQuery(*user, "db1", 1), IsNil)

		results, err = coordinator.ListContinuousQueries(*user, "db1")
		c.Assert(err, IsNil)
		c.Assert(results[0].Points, HasLen, 0)
	}

	// regular database users shouldn't be able to do anything
	for _, user := range disallowedUsers {
		_, err := coordinator.ListContinuousQueries(*user, "db1")
		c.Assert(err, NotNil)
		c.Assert(coordinator.CreateContinuousQuery(*user, "db1", "select * from foo into bar;"), NotNil)
		c.Assert(coordinator.DeleteContinuousQuery(*user, "db1", 1), NotNil)
	}

	coordinator.DeleteDbUser(root, "db1", "db_admin")
	coordinator.DeleteDbUser(root, "db1", "db_user")
}

func (self *CoordinatorSuite) TestDbAdminOperations(c *C) {
	servers := startAndVerifyCluster(3, c)
	defer clean(servers...)

	coordinator := NewCoordinatorImpl(nil, servers[0], servers[0].clusterConfig)

	time.Sleep(REPLICATION_LAG)

	// create a db user
	root, err := coordinator.AuthenticateClusterAdmin("root", "root")
	c.Assert(err, IsNil)
	c.Assert(root.IsClusterAdmin(), Equals, true)
	c.Assert(coordinator.CreateDbUser(root, "db1", "db_user"), IsNil)
	c.Assert(coordinator.ChangeDbUserPassword(root, "db1", "db_user", "db_pass"), IsNil)
	c.Assert(coordinator.SetDbAdmin(root, "db1", "db_user", true), IsNil)
	dbUser, err := coordinator.AuthenticateDbUser("db1", "db_user", "db_pass")
	c.Assert(err, IsNil)

	// Cannot create or delete other cluster admin
	c.Assert(coordinator.CreateClusterAdminUser(dbUser, "another_cluster_admin"), NotNil)
	c.Assert(coordinator.DeleteClusterAdminUser(dbUser, "root"), NotNil)

	// cannot get cluster admin
	_, err = coordinator.ListClusterAdmins(dbUser)
	c.Assert(err, NotNil)

	// can create db users
	c.Assert(coordinator.CreateDbUser(dbUser, "db1", "db_user2"), IsNil)
	c.Assert(coordinator.ChangeDbUserPassword(dbUser, "db1", "db_user2", "db_pass"), IsNil)
	u, err := coordinator.AuthenticateDbUser("db1", "db_user2", "db_pass")
	c.Assert(err, IsNil)
	c.Assert(u.IsClusterAdmin(), Equals, false)
	c.Assert(u.IsDbAdmin("db1"), Equals, false)

	// can get db users
	admins, err := coordinator.ListDbUsers(dbUser, "db1")
	c.Assert(err, IsNil)
	adminsSet := map[string]bool{}
	for _, admin := range admins {
		adminsSet[admin] = true
	}
	c.Assert(adminsSet, DeepEquals, map[string]bool{"db_user": true, "db_user2": true})

	// cannot create db users for a different db
	c.Assert(coordinator.CreateDbUser(dbUser, "db2", "db_user"), NotNil)

	// cannot get db users for a different db
	_, err = coordinator.ListDbUsers(dbUser, "db2")
	c.Assert(err, NotNil)

	// can make db users db admins
	c.Assert(coordinator.SetDbAdmin(dbUser, "db1", "db_user2", true), IsNil)
	u, err = coordinator.AuthenticateDbUser("db1", "db_user2", "db_pass")
	c.Assert(err, IsNil)
	c.Assert(u.IsDbAdmin("db1"), Equals, true)

	// can delete db users
	c.Assert(coordinator.DeleteDbUser(dbUser, "db1", "db_user2"), IsNil)
}

func (self *CoordinatorSuite) TestDbUserOperations(c *C) {
	servers := startAndVerifyCluster(3, c)
	defer clean(servers...)

	coordinator := NewCoordinatorImpl(nil, servers[0], servers[0].clusterConfig)

	time.Sleep(REPLICATION_LAG)

	// create a db user
	root, err := coordinator.AuthenticateClusterAdmin("root", "root")
	c.Assert(err, IsNil)
	c.Assert(root.IsClusterAdmin(), Equals, true)
	c.Assert(coordinator.CreateDbUser(root, "db1", "db_user"), IsNil)
	c.Assert(coordinator.ChangeDbUserPassword(root, "db1", "db_user", "db_pass"), IsNil)
	dbUser, err := coordinator.AuthenticateDbUser("db1", "db_user", "db_pass")
	c.Assert(err, IsNil)

	// Cannot create other cluster admin
	c.Assert(coordinator.CreateClusterAdminUser(dbUser, "another_cluster_admin"), NotNil)

	// can create db users
	c.Assert(coordinator.CreateDbUser(dbUser, "db1", "db_user2"), NotNil)

	// cannot make itself an admin
	c.Assert(coordinator.SetDbAdmin(dbUser, "db1", "db_user", true), NotNil)

	// cannot create db users for a different db
	c.Assert(coordinator.CreateDbUser(dbUser, "db2", "db_user"), NotNil)

	// can change its own password
	c.Assert(coordinator.ChangeDbUserPassword(dbUser, "db1", "db_user", "new_password"), IsNil)
	dbUser, err = coordinator.AuthenticateDbUser("db1", "db_user", "db_pass")
	c.Assert(err, NotNil)
	dbUser, err = coordinator.AuthenticateDbUser("db1", "db_user", "new_password")
	c.Assert(err, IsNil)
}

func (self *CoordinatorSuite) TestUserDataReplication(c *C) {
	servers := startAndVerifyCluster(3, c)
	defer clean(servers...)

	coordinators := make([]*CoordinatorImpl, 0, len(servers))
	for _, server := range servers {
		coordinators = append(coordinators, NewCoordinatorImpl(nil, server, server.clusterConfig))
	}

	// root must exist on all three nodes
	var root User
	var err error
	for _, coordinator := range coordinators {
		root, err = coordinator.AuthenticateClusterAdmin("root", "root")
		c.Assert(err, IsNil)
		c.Assert(root.IsClusterAdmin(), Equals, true)
	}

	c.Assert(coordinators[0].CreateClusterAdminUser(root, "admin"), IsNil)
	time.Sleep(time.Second)
	c.Assert(coordinators[0].ChangeClusterAdminPassword(root, "admin", "admin"), IsNil)
	time.Sleep(REPLICATION_LAG)
	for _, coordinator := range coordinators {
		u, err := coordinator.AuthenticateClusterAdmin("admin", "admin")
		c.Assert(err, IsNil)
		c.Assert(u.IsClusterAdmin(), Equals, true)
	}
}

func (self *CoordinatorSuite) createDatabases(servers []*RaftServer, c *C) {
	err := servers[0].CreateDatabase("db1", 0)
	c.Assert(err, IsNil)
	err = servers[1].CreateDatabase("db2", 1)
	c.Assert(err, IsNil)
	err = servers[2].CreateDatabase("db3", 3)
	c.Assert(err, IsNil)
}

func (self *CoordinatorSuite) TestCanCreateDatabaseWithNameAndReplicationFactor(c *C) {
	servers := startAndVerifyCluster(3, c)
	defer clean(servers...)

	self.createDatabases(servers, c)

	time.Sleep(REPLICATION_LAG)

	for i := 0; i < 3; i++ {
		databases := servers[i].clusterConfig.databaseReplicationFactors
		c.Assert(databases, DeepEquals, map[string]uint8{
			"db1": 1,
			"db2": 1,
			"db3": 3,
		})
	}

	err := servers[0].CreateDatabase("db3", 1)
	c.Assert(err, ErrorMatches, ".*db3 exists.*")
	err = servers[2].CreateDatabase("db3", 1)
	c.Assert(err, ErrorMatches, ".*db3 exists.*")
}

func (self *CoordinatorSuite) TestCanDropDatabaseWithName(c *C) {
	servers := startAndVerifyCluster(3, c)
	defer clean(servers...)

	self.createDatabases(servers, c)

	err := servers[0].DropDatabase("db1")
	c.Assert(err, IsNil)
	err = servers[1].DropDatabase("db2")
	c.Assert(err, IsNil)
	err = servers[2].DropDatabase("db3")
	c.Assert(err, IsNil)

	time.Sleep(REPLICATION_LAG)

	for i := 0; i < 3; i++ {
		databases := servers[i].clusterConfig.GetDatabases()
		c.Assert(databases, HasLen, 0)
	}

	err = servers[0].DropDatabase("db3")
	c.Assert(err, ErrorMatches, ".*db3 doesn't exist.*")
	err = servers[2].DropDatabase("db3")
	c.Assert(err, ErrorMatches, ".*db3 doesn't exist.*")
}

func (self *CoordinatorSuite) TestCheckReadAccess(c *C) {
	datastoreMock := &DatastoreMock{}
	coordinator := NewCoordinatorImpl(datastoreMock, nil, nil)
	mock := `{
    "points": [
      {
        "values": [
          {
            "int64_value": 3
          }
        ],
        "sequence_number": 1,
        "timestamp": 23423
      }
    ],
    "name": "foo",
    "fields": ["value"]
  }`
	series := stringToSeries(mock, c)
	user := &MockUser{
		dbCannotWrite: map[string]bool{"foo": true},
	}
	err := coordinator.WriteSeriesData(user, "foo", series)
	c.Assert(err, ErrorMatches, ".*Insufficient permission.*")
	c.Assert(datastoreMock.Series, IsNil)
}

func (self *CoordinatorSuite) TestServersGetUniqueIdsAndCanActivateCluster(c *C) {
	servers := startAndVerifyCluster(3, c)
	defer clean(servers...)

	// ensure they're all in the same order across the cluster
	expectedServers := servers[0].clusterConfig.servers
	for _, server := range servers {
		c.Assert(server.clusterConfig.servers, HasLen, len(expectedServers))
		for i, clusterServer := range expectedServers {
			c.Assert(server.clusterConfig.servers[i].Id, Equals, clusterServer.Id)
		}
	}
	// ensure cluster server ids are unique
	idMap := make(map[uint32]bool)
	for _, clusterServer := range servers[0].clusterConfig.servers {
		_, ok := idMap[clusterServer.Id]
		c.Assert(ok, Equals, false)
		idMap[clusterServer.Id] = true
	}
}

func (self *CoordinatorSuite) TestCanJoinAClusterWhenNotInitiallyPointedAtLeader(c *C) {
	servers := startAndVerifyCluster(2, c)
	defer clean(servers...)
	newServer := newConfigAndServer(c)
	defer clean(newServer)
	l, err := net.Listen("tcp4", ":0")
	c.Assert(err, IsNil)
	leaderAddr, ok := servers[1].leaderConnectString()
	c.Assert(ok, Equals, true)
	leaderPort, _ := strconv.Atoi(strings.Split(leaderAddr, ":")[2])
	followerPort := servers[1].port
	if leaderPort == servers[1].port {
		followerPort = servers[0].port
	}
	newServer.config.SeedServers = []string{fmt.Sprintf("http://localhost:%d", followerPort)}
	newServer.Serve(l)
	time.Sleep(SERVER_STARTUP_TIME)

	err = servers[0].CreateDatabase("db8", uint8(1))
	c.Assert(err, Equals, nil)
	time.Sleep(REPLICATION_LAG)
	assertConfigContains(newServer.port, "db8", true, c)
}
