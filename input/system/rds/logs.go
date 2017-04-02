package rds

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/pganalyze/collector/config"
	"github.com/pganalyze/collector/state"
	"github.com/pganalyze/collector/util/awsutil"
)

// GetLogLines - Gets log lines for an Amazon RDS instance
func GetLogLines(config config.ServerConfig) (result []state.LogLine, samples []state.PostgresQuerySample) {
	sess := awsutil.GetAwsSession(config)

	rdsSvc := rds.New(sess)

	instance, err := awsutil.FindRdsInstance(config, sess)

	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	// Retrieve all possibly matching logfiles in the last 10 minutes
	// TODO: Use prevState here instead to get the last collectedAt
	linesNewerThan := time.Now().Add(-10 * time.Minute)
	lastWritten := linesNewerThan.Unix() * 1000

	params := &rds.DescribeDBLogFilesInput{
		DBInstanceIdentifier: instance.DBInstanceIdentifier,
		FileLastWritten:      &lastWritten,
	}

	resp, err := rdsSvc.DescribeDBLogFiles(params)

	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	for _, logFile := range resp.DescribeDBLogFiles {
		lastMarker := aws.String("0")

		for {
			params := &rds.DownloadDBLogFilePortionInput{
				DBInstanceIdentifier: instance.DBInstanceIdentifier,
				LogFileName:          logFile.LogFileName,
				Marker:               lastMarker,
			}

			resp, err := rdsSvc.DownloadDBLogFilePortion(params)

			if err != nil {
				// TODO: Check for unauthorized error:
				// Error: AccessDenied: User: arn:aws:iam::XXX:user/pganalyze_collector is not authorized to perform: rds:DownloadDBLogFilePortion on resource: arn:aws:rds:us-east-1:XXX:db:XXX
				// status code: 403, request id: XXX
				fmt.Printf("Error: %v\n", err)
				return
			}

			var logLines []state.LogLine

			var incompleteLine = false

			reader := bufio.NewReader(strings.NewReader(*resp.LogFileData))
			for {
				line, isPrefix, err := reader.ReadLine()
				if err == io.EOF {
					break
				}

				if err != nil {
					fmt.Printf("Error: %v\n", err)
					break
				}

				if incompleteLine {
					if len(logLines) > 0 {
						logLines[len(logLines)-1].Content += string(line)
					}
					incompleteLine = isPrefix
					continue
				}

				incompleteLine = isPrefix

				var logLine state.LogLine

				// log_line_prefix is always "%t:%r:%u@%d:[%p]:" on RDS
				parts := strings.SplitN(string(line), ":", 8)
				if len(parts) != 8 {
					if len(logLines) > 0 {
						logLines[len(logLines)-1].Content += string(line)
					}
					continue
				}

				timestamp, err := time.Parse("2006-01-02 15:04:05 MST", parts[0]+":"+parts[1]+":"+parts[2])
				if err != nil {
					if len(logLines) > 0 {
						logLines[len(logLines)-1].Content += string(line)
					}
					continue
				}

				userDbParts := strings.SplitN(parts[4], "@", 2)
				if len(userDbParts) == 2 {
					logLine.Username = userDbParts[0]
					logLine.Database = userDbParts[1]
				}

				hostnamePortParts := strings.SplitN(parts[3], "(", 2)
				if len(hostnamePortParts) == 2 {
					logLine.ClientHostname = hostnamePortParts[0]
					clientPort, _ := strconv.Atoi(strings.TrimRight(hostnamePortParts[1], ")"))
					logLine.ClientPort = int32(clientPort)
				}

				logLine.OccurredAt = timestamp
				backendPid, _ := strconv.Atoi(parts[5][1 : len(parts[5])-1])
				logLine.BackendPid = int32(backendPid)
				logLine.LogLevel = parts[6]
				logLine.Content = strings.TrimLeft(parts[7], " ")

				logLines = append(logLines, logLine)
			}

			// Split log lines by backend to ensure we have the right context
			backendLogLines := make(map[int32][]state.LogLine)

			for _, logLine := range logLines {
				// Ignore loglines which are outside our time window
				if logLine.OccurredAt.Before(linesNewerThan) {
					continue
				}

				backendLogLines[logLine.BackendPid] = append(backendLogLines[logLine.BackendPid], logLine)
			}

			skipLines := 0

			for _, logLines := range backendLogLines {
				for idx, logLine := range logLines {
					if skipLines > 0 {
						skipLines--
						continue
					}

					// Look up to 2 lines in the future to find context for this line
					lowerBound := int(math.Min(float64(len(logLines)), float64(idx+1)))
					upperBound := int(math.Min(float64(len(logLines)), float64(idx+3)))
					for _, futureLine := range logLines[lowerBound:upperBound] {
						if futureLine.LogLevel == "STATEMENT" || futureLine.LogLevel == "DETAIL" || futureLine.LogLevel == "HINT" {
							if futureLine.LogLevel == "STATEMENT" && !strings.HasSuffix(futureLine.Content, "[Your log message was truncated]") {
								logLine.Query = futureLine.Content
							}
							logLine.AdditionalLines = append(logLine.AdditionalLines, futureLine)
							skipLines++
						} else {
							break
						}
					}

					if strings.HasPrefix(logLine.Content, "duration: ") {
						if !strings.HasSuffix(logLine.Content, "[Your log message was truncated]") {
							parts := regexp.MustCompile(`duration: ([\d\.]+) ms([^:]+): (.+)`).FindStringSubmatch(logLine.Content)

							if len(parts) == 4 {
								logLine.Query = parts[3]

								if !strings.Contains(parts[2], "bind") && !strings.Contains(parts[2], "parse") {
									runtime, _ := strconv.ParseFloat(parts[1], 64)
									samples = append(samples, state.PostgresQuerySample{
										OccurredAt: logLine.OccurredAt,
										Username:   logLine.Username,
										Database:   logLine.Database,
										Query:      logLine.Query,
										RuntimeMs:  runtime,
									})
								}
							}
						}
					}

					// TODO: Add privacy mode option
					// * Clean STATEMENT and "duration: " contents
					// * Remove DETAIL "parameters: "

					result = append(result, logLine)
				}
			}

			lastMarker = resp.Marker
			if !*resp.AdditionalDataPending {
				break
			}
		}
	}

	return
}
