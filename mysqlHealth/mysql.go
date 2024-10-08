package mysqlHealth

import (
    "os"
    "fmt"
    "regexp"
    "os/exec"
    "strings"
    "database/sql"
    "github.com/go-ini/ini"
    _ "github.com/go-sql-driver/mysql"
    "github.com/monobilisim/monokit/common"
    issues "github.com/monobilisim/monokit/common/redmine/issues"
)

var Connection *sql.DB

func mariadbOrMysql() string {
    _, err := exec.LookPath("mysql")
    if err != nil {
        return "mariadb"
    }
    return "mysql"
}

func FindMyCnf() []string {
    cmd := exec.Command(mariadbOrMysql() + "d", "--verbose", "--help")
    output, err := cmd.CombinedOutput()
    if err != nil {
        common.LogError("Error running " + mariadbOrMysql() + "d command:" + err.Error())
        return nil
    }

    lines := strings.Split(string(output), "\n")
    foundDefaultOptions := false
    for _, line := range lines {
        if strings.Contains(line, "Default options") {
            foundDefaultOptions = true
            continue
        }

        if foundDefaultOptions {

            return strings.Fields(strings.Replace(line, "~", os.Getenv("HOME"), 1))
        }
    }

    return nil
}

func MyCnf(profile string) (string, error) {
    var host, port, dbname, user, password, socket string
    var found bool

    for _, path := range FindMyCnf() {
        if _, err := os.Stat(path); err == nil {
            cfg, err := ini.LoadSources(ini.LoadOptions{AllowBooleanKeys: true}, path)
            if err != nil {
                return "", fmt.Errorf("error loading config file %s: %w", path, err)
            }

            for _, s := range cfg.Sections() {
                if profile != "" && s.Name() != profile {
                    continue
                }

                found = true

                host = s.Key("host").String()
                port = s.Key("port").String()
                dbname = s.Key("dbname").String()
                user = s.Key("user").String()
                password = s.Key("password").String()
                socket = s.Key("socket").String()

                // Break after finding the first matching profile
                break
            }
        }
    }

    if !found {
        return "", fmt.Errorf("no matching entry found for profile %s", profile)
    }

    if socket != "" {
        return fmt.Sprintf("%s:%s@unix(%s)/%s", user, password, socket, dbname), nil
    }

    if port == "" {
        port = "3306"
    }

    return fmt.Sprintf("%s:%s@tcp(%s:%s)/%s", user, password, host, port, dbname), nil
}

func Connect() {
    connStr, err := MyCnf("client")
    if err != nil {
        common.LogError(err.Error())
        return
    }

    db, err := sql.Open("mysql", connStr)
    if err != nil {
        common.LogError("Error connecting to database: " + err.Error())
        return
    }

    Connection = db
}

func SelectNow() {
    // Simple query to check if the connection is working
    rows, err := Connection.Query("SELECT NOW()")
    defer rows.Close()
    if err != nil {
        common.LogError("Error querying database: " + err.Error())
        common.AlarmCheckDown("now", "Couldn't run a 'SELECT' statement on MySQL", false)
        common.PrettyPrintStr("MySQL", false, "accessible")
        return
    } else {
        common.AlarmCheckUp("now", "Can run 'SELECT' statements again", false)
        common.PrettyPrintStr("MySQL", true, "accessible")
    }
}

func CheckProcessCount() {
    rows, err := Connection.Query("SHOW PROCESSLIST")
    defer rows.Close()
    if err != nil {
        common.LogError("Error querying database: " + err.Error())
        common.AlarmCheckDown("processlist", "Couldn't run a 'SHOW PROCESSLIST' statement on MySQL", false)
        common.PrettyPrintStr("Number of Processes", false, "accessible")
        return
    } else {
        common.AlarmCheckUp("processlist", "Can run 'SHOW PROCESSLIST' statements again", false)
    }

    // Count the number of processes

    var count int

    for rows.Next() {
        count++
    }

    if count > DbHealthConfig.Mysql.Process_limit {
        common.AlarmCheckDown("processcount", fmt.Sprintf("Number of MySQL processes is over the limit: %d", count), false)
        common.PrettyPrint("Number of Processes", "", float64(count), false, false, true, float64(DbHealthConfig.Mysql.Process_limit))
    } else {
        common.AlarmCheckUp("processcount", "Number of MySQL processes is under the limit", false)
        common.PrettyPrint("Number of Processes", "", float64(count), false, false, true, float64(DbHealthConfig.Mysql.Process_limit))
    }
}

func InaccessibleClusters() {
    rows := Connection.QueryRow("SHOW STATUS WHERE Variable_name = 'wsrep_incoming_addresses'")

    var ignored string
    var listening_clusters string
    var listening_clusters_array []string

    if err := rows.Scan(&ignored, &listening_clusters); err != nil {
        common.LogError("Error querying database: " + err.Error())
        return
    }

    listening_clusters_array = strings.Split(listening_clusters, ",")

    if len(listening_clusters_array) == 0 {
        return
    }

    // Check if common.TmpDir + /cluster_nodes exists
    if _, err := os.Stat(common.TmpDir + "/cluster_nodes"); err == nil { 
        // If it exists, read the file and compare the contents
        file, err := os.Open(common.TmpDir + "/cluster_nodes")
        if err != nil {
            common.LogError("Error opening file: " + err.Error())
            return
        }
        // Split it and make it into an array
        file_contents := make([]byte, 1024)
        count, err := file.Read(file_contents)
        if err != nil {
            common.LogError("Error reading file: " + err.Error())
            return
        }

        file.Close()

        file_contents_array := strings.Split(string(file_contents[:count]), ",")

        // Compare the two arrays
        for _, cluster := range file_contents_array {
            if common.IsInArray(cluster, listening_clusters_array) {
                common.AlarmCheckUp(cluster, "Node " + cluster + " is back in cluster.", true)
            } else {
                common.AlarmCheckDown(cluster, "Node " + cluster + " is no longer in the cluster.", true)
            }
        }
    }

    // Create a file with the cluster nodes
    common.WriteToFile(common.TmpDir + "/cluster_nodes", listening_clusters)
    
}

func CheckClusterStatus() {
    rows := Connection.QueryRow("SHOW STATUS WHERE Variable_name = 'wsrep_cluster_size'")

    var ignored string
    var cluster_size int
    var identifierRedmine string

    // Split the identifier into two parts using a hyphen
	identifierParts := strings.Split(common.Config.Identifier, "-")
	if len(identifierParts) >= 2 {
		identifierRedmine = strings.Join(identifierParts[:2], "-")

		// Check if the identifier is the same as the first two parts
		if common.Config.Identifier == identifierRedmine {
			// Remove all numbers from the end of the string
			re := regexp.MustCompile("[0-9]*$")
			identifierRedmine = re.ReplaceAllString(identifierRedmine, "")
		}

	}

    if err := rows.Scan(&ignored, &cluster_size); err != nil {
        common.LogError("Error querying database: " + err.Error())
        return
    }
    
    var varname string
    var value string
    rows = Connection.QueryRow("SHOW STATUS WHERE Variable_name = 'wsrep_cluster_size'")

    if err := rows.Scan(&varname, &value); err != nil {
        common.LogError("Error querying database: " + err.Error())
        return
    }


    if cluster_size == DbHealthConfig.Mysql.Cluster.Size {
        common.AlarmCheckUp("cluster_size", "Cluster size is accurate: " + fmt.Sprintf("%d", cluster_size) + "/" + fmt.Sprintf("%d", DbHealthConfig.Mysql.Cluster.Size), false)
        issues.CheckUp("cluster-size", "MySQL Cluster boyutu: " + string(cluster_size) + " - " + common.Config.Identifier + "\n`" + varname + ": " + value + "`")
        common.PrettyPrint("Cluster Size", "", float64(cluster_size), false, false, true, float64(DbHealthConfig.Mysql.Cluster.Size))
    } else if cluster_size == 0 {
        common.AlarmCheckDown("cluster_size", "Couldn't get cluster size", false)
        common.PrettyPrintStr("Cluster Size", true, "Unknown")
        issues.Update("cluster-size", "`SHOW STATUS WHERE Variable_name = 'wsrep_cluster_size'` sorgusunda cluster boyutu alınamadı.", true)
    } else {
        common.AlarmCheckDown("cluster_size", "Cluster size is not accurate: " + fmt.Sprintf("%d", cluster_size) + "/" + fmt.Sprintf("%d", DbHealthConfig.Mysql.Cluster.Size), false)
        issues.Update("cluster-size", "MySQL Cluster boyutu: " + string(cluster_size) + " - " + common.Config.Identifier + "\n`" + varname + ": " + value + "`", true)
        common.PrettyPrint("Cluster Size", "", float64(cluster_size), false, false, true, float64(DbHealthConfig.Mysql.Cluster.Size))
    }


    if cluster_size == 1 || cluster_size > DbHealthConfig.Mysql.Cluster.Size {
        
        issueIdIfExists := issues.Exists("MySQL Cluster boyutu: " + string(cluster_size) + " - " + identifierRedmine, "", false)

        if _, err := os.Stat(common.TmpDir + "/mysql-cluster-size-redmine.log"); err == nil && issueIdIfExists == "" {
            common.WriteToFile(common.TmpDir + "/mysql-cluster-size-redmine.log", issueIdIfExists)
        }

        issues.CheckDown("cluster-size", "MySQL Cluster boyutu: " + string(cluster_size) + " - " + identifierRedmine, "MySQL Cluster boyutu: " + string(cluster_size) + " - " + common.Config.Identifier + "\n`" + varname + ": " + value + "`")
    }
}

func CheckNodeStatus() {
    rows := Connection.QueryRow("SHOW STATUS WHERE Variable_name = 'wsrep_ready'")

    var name string
    var status string

    if err := rows.Scan(&name, &status); err != nil {
        common.LogError("Error querying database: " + err.Error())
        return
    }

    if name == "" && status == "" {
        common.AlarmCheckDown("node_status", "Couldn't get node status", false)
        common.PrettyPrintStr("Node Status", true, "Unknown")
    } else if status == "ON" {
        common.AlarmCheckUp("node_status", "Node status is 'ON'", false)
        common.PrettyPrintStr("Node Status", true, "ON")
    } else {
        common.AlarmCheckDown("node_status", "Node status is '" + status + "'", false)
        common.PrettyPrintStr("Node Status", false, "ON")
    }
}

func CheckClusterSynced() {
    rows := Connection.QueryRow("SHOW STATUS WHERE Variable_name = 'wsrep_local_state_comment'")
    
    var name string
    var status string

    if err := rows.Scan(&name, &status); err != nil {
        common.LogError("Error querying database: " + err.Error())
        return
    }

    if name == "" && status == "" {
        common.AlarmCheckDown("cluster_synced", "Couldn't get cluster synced status", false)
        common.PrettyPrintStr("Cluster sync state", true, "Unknown")
    } else if status == "Synced" {
        common.AlarmCheckUp("cluster_synced", "Cluster is synced", false)
        common.PrettyPrintStr("Cluster sync state", true, "Synced")
    } else {
        common.AlarmCheckDown("cluster_synced", "Cluster is not synced, state: " + status, false)
        common.PrettyPrintStr("Cluster sync state", false, "Synced")
    }
}

func CheckDB() {
    cmd := exec.Command(mariadbOrMysql(), "--auto-repair", "--all-databases")
    output, err := cmd.CombinedOutput()
    if err != nil {
        common.LogError("Error running " + mariadbOrMysql() + " command:" + err.Error())
        return
    }

    lines := strings.Split(string(output), "\n")
    tables := make([]string, 0)
    repairingTables := false
    for _, line := range lines {
        if strings.Contains(line, "Repairing tables") {
            repairingTables = true
            continue
        }
        if repairingTables && !strings.HasPrefix(line, " ") {
            tables = append(tables, line)
        }
    }

    if len(tables) > 0 {
        message := fmt.Sprintf("[MySQL - %s] [:info:] MySQL - `%s` result\n", common.Config.Identifier, mariadbOrMysql)
        for _, table := range tables {
            message += table + "\n"
        }
        common.Alarm(message)
    }
}
