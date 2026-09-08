package robot_test

// The owned_bots query reads agent_hosting, whose schema migration is owned by
// botfather. Import it from the external test package so robot's integration
// test database receives that migration without creating an import cycle.
import _ "github.com/Mininglamp-OSS/octo-server/modules/botfather"
