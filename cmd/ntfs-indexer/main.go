// ntfs-indexer — the NTFS index service binary (mini-mcp module). One
// binary, two roles (PRD §3): --role agent runs on each file server (MFT/USN
// scan, pushes batches to the hub), --role hub runs on the ZCode server
// (sole SQLite writer, LDAP enrichment, folder tags).
//
// Э-0.1 (PRD §13) ships the schema migrator only: --migrate creates or
// upgrades the SQLite index and exits. The agent/hub loops land with
// Э-0.2…Э-0.4. This is a service/CLI binary, not an MCP stdio server —
// stdout is not protocol-framed here, but logging still goes to stderr.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"mini-mcp/internal/ntfs"
)

func main() {
	log.SetFlags(log.LstdFlags)
	log.SetOutput(os.Stderr)

	role := flag.String("role", "hub", "indexer role: agent (file server) or hub (ZCode server)")
	dbPath := flag.String("db", "", "path to the SQLite index (required)")
	doMigrate := flag.Bool("migrate", false, "apply schema migrations, print stats, exit")
	scanVol := flag.String("scan", "", "dry MFT snapshot (Э-0.2): volume device, e.g. 'E:'; prints assembled paths, no DB writes; needs an elevated console (П-2)")
	include := flag.String("include", "", "with --scan/--volume: volume-rooted subpath to keep, e.g. 'Users\\wsl\\Desktop\\тест'; empty = whole volume")
	list := flag.Bool("list", false, "with --scan: print ALL matched paths (diff against `dir /s /b`)")
	volume := flag.String("volume", "", "agent mode (Э-0.3): volume device 'C:' — snapshot import + USN delta loop into --db (elevated console, П-2)")
	watch := flag.Int("watch", 0, "agent mode: poll interval seconds; 0 = single pass and exit (pilot verification)")
	agentName := flag.String("agent-name", "", "agent identity stored in roots.agent (default: hostname)")
	uncBase := flag.String("unc-base", "", "canonical base path for issuance (default: volume root, e.g. 'C:\\')")
	flag.Parse()

	if *scanVol != "" {
		if err := runScan(*scanVol, *include, *list); err != nil {
			log.Fatalf("scan: %v", err)
		}
		return
	}

	if *volume != "" {
		if *dbPath == "" {
			log.Fatal("--volume agent mode requires --db")
		}
		if *agentName == "" {
			if h, err := os.Hostname(); err == nil {
				*agentName = h
			} else {
				*agentName = "local"
			}
		}
		if *uncBase == "" {
			*uncBase = *volume + `\`
		}
		if err := runAgent(*dbPath, *volume, *include, *agentName, *uncBase, time.Duration(*watch)*time.Second); err != nil {
			log.Fatalf("agent: %v", err)
		}
		return
	}

	if *dbPath == "" {
		log.Fatal("--db is required")
	}
	if *role != "agent" && *role != "hub" {
		log.Fatalf("--role must be agent or hub, got %q", *role)
	}
	if *doMigrate {
		db, err := ntfs.Open(*dbPath)
		if err != nil {
			log.Fatalf("migrate %s: %v", *dbPath, err)
		}
		defer db.Close()
		v, roots, files, err := ntfs.SchemaInfo(db)
		if err != nil {
			log.Fatalf("schema info: %v", err)
		}
		fmt.Printf("migrated %s: schema_version=%d roots=%d files=%d\n", *dbPath, v, roots, files)
		return
	}
	log.Fatalf("--role %s: agent/hub loops are not implemented yet (Э-0.2+)", *role)
}
