package main

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/viper"
)

func runLocalCmd(runCmd string) (string, error) {
	cmd := exec.Command("bash", "-c", runCmd)
	var stdout, stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stdout
	fmt.Printf("Run [%s]\n", cmd.String())
	if err := cmd.Run(); err != nil {
		return "", errors.New(stderr.String())
	}
	return stdout.String(), nil
}
func mustRunLocalCmd(runCmd string) string {
	out, err := runLocalCmd(runCmd)
	if err != nil {
		panic(err)
	}
	return out
}

func buildPsqlCmd(db DbConfig, runDB, runCmd string) string {
	return fmt.Sprintf(
		`PGPASSWORD=%s psql -h %s -p %s -U %s -d %s -c "%s"`,
		db.Password,
		db.Host,
		db.Port,
		db.Username,
		runDB,
		runCmd,
	)
}

func buildDumpCmd(db DbConfig, filename string) string {
	return fmt.Sprintf(
		"PGPASSWORD=%s pg_dump -h %s -p %s -U %s -d %s %s -f %s",
		db.Password,
		db.Host,
		db.Port,
		db.Username,
		db.Database,
		"-Fc -x",
		filename,
	)
}

func buildRestoreCmd(db DbConfig, database, filename string) string {
	return fmt.Sprintf(
		"PGPASSWORD=%s pg_restore -h %s -p %s -U %s -d %s %s %s",
		db.Password,
		db.Host,
		db.Port,
		db.Username,
		database,
		"-x -O -c --if-exists",
		filename,
	)
}

type DbConfig struct {
	Host     string
	Port     string
	Database string
	Username string
	Password string
}

type Config struct {
	Namespace     string   `mapstructure:"namespace"`
	PodPrefix     string   `mapstructure:"pod_prefix"`
	CredentialCmd string   `mapstructure:"credential_cmd"`
	LocalDb       DbConfig `mapstructure:"local_db"`
	RemoteDb      DbConfig `mapstructure:"remote_db"`
	access        string
	pod           string
}

var config Config

func init() {
	viper.AddConfigPath(".")
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	err := viper.ReadInConfig()
	if err != nil {
		panic(err)
	}

	if err := viper.Unmarshal(&config); err != nil {
		panic(err)
	}

	config.access = mustGetAccess()
	pod := mustRunKubectlCmd(fmt.Sprintf("get pods | grep %s | grep Running | awk '{ print $1 }'", config.PodPrefix))
	config.pod = strings.TrimSpace(pod)

	fmt.Println("-> config", config)
}

// depend on config
func mustGetAccess() string {
	mustRunLocalCmd("aws sts get-caller-identity")
	cred := mustRunLocalCmd(config.CredentialCmd)
	cred = regexp.MustCompile(`(?m)\s`).ReplaceAllString(cred, "")
	r := regexp.MustCompile(`"AccessKeyId":"(.+)","SecretAccessKey":"(.+)","SessionToken":"(.+)","Expiration"`)
	match := r.FindAllStringSubmatch(cred, -1)
	access := fmt.Sprintf(`AWS_ACCESS_KEY_ID="%s" AWS_SECRET_ACCESS_KEY="%s" AWS_SESSION_TOKEN="%s"`, match[0][1], match[0][2], match[0][3])
	return access
}

// depend on config
func mustRunKubectlCmd(runCmd string) string {
	return mustRunLocalCmd(fmt.Sprintf("%s kubectl -n %s %s", config.access, config.Namespace, runCmd))
}

// depend on config
func mustRunPodCmd(runCmd string) string {
	return mustRunLocalCmd(fmt.Sprintf(`%s kubectl exec -it %s -n %s -- bash -c '%s'`, config.access, config.pod, config.Namespace, runCmd))
}

func main() {
	out := mustRunPodCmd("apt-get update && apt-get install -y lsb-release")
	fmt.Println(out)

	out = mustRunPodCmd(`echo "deb http://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" > /etc/apt/sources.list.d/pgdg.list`)
	fmt.Println(out)

	out = mustRunPodCmd("wget --quiet -O - https://www.postgresql.org/media/keys/ACCC4CF8.asc | apt-key add -")
	fmt.Println(out)

	out = mustRunPodCmd("apt-get update && apt-get -y install postgresql-client-12")
	fmt.Println(out)

	ts := time.Now().Unix()
	backupFile := fmt.Sprintf("dump_%d.sql", ts)
	backupFileLocal := fmt.Sprintf("~/%s", backupFile)
	out = mustRunPodCmd(buildDumpCmd(config.RemoteDb, backupFile))
	fmt.Println(out)
	defer func() {
		out = mustRunPodCmd(fmt.Sprintf("rm -f %s", backupFile))
		fmt.Println(out)
	}()

	out = mustRunKubectlCmd(fmt.Sprintf("cp %s:/www/yield-engine/%s %s", config.pod, backupFile, backupFileLocal))
	fmt.Println(out)
	defer func() {
		out = mustRunLocalCmd(fmt.Sprintf("rm -f %s", backupFileLocal))
		fmt.Println(out)
	}()

	restoreDb := fmt.Sprintf("%s_r_%d", config.LocalDb.Database, ts)
	out = mustRunLocalCmd(buildPsqlCmd(config.LocalDb, config.LocalDb.Database, fmt.Sprintf("CREATE DATABASE %s", restoreDb)))
	fmt.Println(out)
	defer func() {
		out = mustRunLocalCmd(buildPsqlCmd(config.LocalDb, config.LocalDb.Database, fmt.Sprintf("DROP DATABASE IF EXISTS %s", restoreDb)))
		fmt.Println(out)
	}()

	out, err := runLocalCmd(buildRestoreCmd(config.LocalDb, restoreDb, backupFileLocal))
	if err != nil {
		fmt.Printf("\n***WARNING RESTORE ERROR: %s\n", err)
	}
	fmt.Println(out)

	oldDb := fmt.Sprintf("%s_%d", config.LocalDb.Database, ts)
	out = mustRunLocalCmd(buildPsqlCmd(config.LocalDb, restoreDb, fmt.Sprintf("ALTER DATABASE %s RENAME TO %s", config.LocalDb.Database, oldDb)))
	fmt.Println(out)
	defer func() {
		out = mustRunLocalCmd(buildPsqlCmd(config.LocalDb, config.LocalDb.Database, fmt.Sprintf("DROP DATABASE %s", oldDb)))
		fmt.Println(out)
	}()

	out = mustRunLocalCmd(buildPsqlCmd(config.LocalDb, oldDb, fmt.Sprintf("ALTER DATABASE %s RENAME TO %s", restoreDb, config.LocalDb.Database)))
	fmt.Println(out)
}
