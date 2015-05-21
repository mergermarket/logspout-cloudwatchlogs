# logspout-cloudwatchlogs

Custom Logspout adaptor for sending logs to CloudWatchLogs

# API

Logspout handles taking a "route" that corresponds to this adapter and handles passing logs to it. This can be passed as an argument to the logstash process as follows:

    'cloudwatchlogs://AWSREGION?option1=value1&option2=value2'

For example, the following sends all container logs to a "all-the-logs" log group in the eu-west-1 region (maybe not the best idea):

    'cloudwatchlogs://eu-west-1?stream-prefix=i-d34db33f&default-log-group=other-logs'

## Scheme

The URL scheme should be set to "cloudwatchlogs" in order for the adapter to handle log messages.

## Address

The URL address is used to set the AWS region to use when connecting to CloudWatchLogs - e.g. "eu-west-1" or "us-east-1".

## Options

### Global options

#### `stream-prefix` (required)

CloudWatch Streams (within a Log Group) are designed to accept a steam of logs from a single source. To enforce this calls to put logs return a sequence number that must be provided with a subsequent call to put logs. A missing or incorrect sequence number results in an error. This error does contain the correct sequence number so it is possible to recover from this error, but it is a bad idea to abuse this to effect interleaving logs from multiple sources: doing so would result in a race condition between multiple log sources competing to send log events, each causing a denial of service in the other(s).

From a single container host this adapter keeps the stream names unique by including the container name and source (i.e. "stdout" or "stderr"). However as it is possible that containers on separate hosts will have the same name, a unique prefix is applied - from the "stream-prefix" parameter. It is therefore important that this value is kept unique - good values for this would be the hostname or machine identifier for the container host (e.g. EC2 instance identifier). The format of the stream name is:

    "{stream-prefix}-{container-name}-{stream}"

For example, if you provide the `stream-prefix` "i-d34db33f" then a container called "complaining-harrold" would result in streams called "i-d34db33f-complaining-harrold-stdout" and "i-d34db33f-complaining-harrold-stderr".

### Log group selection

* At least on of `default-log-group`, `log-group-from-env` or `container-log-groups` is required in order for any logs to be sent to CloudWatchLogs.*

#### `default-log-group=LogGroupName`

For containers not matched by one of other mechanisms described here, logs will be sent to this log group.

#### `log-group-from-env=on`

This adapter supports discovering the CloudWatchLogs Log Group from an environment variable or environment variables within the container. To enable this, supply this parameter:

    'cloudwatchlogs://eu-west-1?log-group-from-env=on'

This will cause the adapter to look for the following environment variable(s):

* `LOGSPOUT_CLOUDWATCHLOGS_LOG_GROUP` - logs sent to both STDOUT AND STDERR will be forwarded to the log group identified by the value of this environment variable.
* `LOGSPOUT_CLOUDWATCHLOGS_LOG_GROUP_STDOUT` - logs sent to STDOUT will be forwarded to the log group identified by the value fo this environment variable.
* `LOGSPOUT_CLOUDWATCHLOGS_LOG_GROUP_STDERR` - logs sent to STDERR will be forwarded to the log group identified by the value fo this environment variable.

When enabled, it is a error for a container to specify both `LOGSPOUT_CLOUDWATCHLOGS_LOG_GROUP` and either `LOGSPOUT_CLOUDWATCHLOGS_LOG_GROUP_STDOUT` or `LOGSPOUT_CLOUDWATCHLOGS_LOG_GROUP_STDERR`. Where this is the case, the error will be logged to the logging container's own STDERR and the container's logs will be ignored.

#### `container-log-groups={"container-name1":{"both":"LogGroupName1"},"container-name2":{"stdout":"LogGroupName2-stdout","stderr":"LogGroupName2-stderr"}}`

JSON document that maps specific container names to log groups. For examle, if you have containers called "ecs-agent" and "monitoring-agent", logs from these containers can be mapped to log groups with:

    'cloudwatchlogs://eu-west-1?container-log-groups={"ecs-agent":{"both":"ECSAgentLogGroup"}%2C"monitoring-agent":{"both":"MonitoringAgentLogGroup"}}'

The STDOUT and STDERR stream from each container can be mapped to different log groups (by including "stdout" and/or "stderr"), or they can both be routed to the same log group (by including "both"). It is an error to include "both" and either "stdout" or "stderr". Doing so will prevent the logging container from starting.

It is however legitimate to include neither "both" nor "stdout" or "stderr", or just "stdout" or "stderr", which will result in preventing logs from the absent streams being shipped. For example, settting `container-log-groups={"chatty-service":{}}` will result in no logs for the "chatty-service" to be shipped, and settting `container-log-groups={"hungry-bridget":{"stderr":"MyLogGroup"}}` will cause just STDERR to be shipped for the "hungry-bridget" service.

*Please not that "," (comma) must be replaced with "%2C" in order for the JSON to be passed through to the adaptor. If in doubt, it may be wise to pass the entire JSON document through `encodeURIComponent`.*

# Developing

I haven't found a particularly good way to test logspout adapters during development. In order to shorten the cycle time of making a change and testing it, you can do the following:

    # start the development container
    ./dev.sh
    
    # initialise the environment
    /logspout-cloudwatchlogs/init.sh
    
    # build and run logspout with the adapter
    /logspout-cloudwatchlogs/run.sh cloudwatchlogs://eu-west-1

Since the first two steps take quite a while, this allows you to make code changes and iterate more rapidly (just running the third command). Suggestions on a better way to do this welcome (particularly where we don't have to duplicate the build steps, which are contained in logspout proper).

## Future improvements

The following are areas that need to be improved (i.e. a to do list):

* Handle rate limiting - back off and retry.
* Handle batching of messages, with configurable maximum wait time (default 5 seconds) and batch size (default 100).
* Have a configurable per stream maximum buffer before messages are dropped on the floor, in order to allow bounds to be placed on memory size. Log messages being dropped.
* Send aggregate statistics about log delivery as CloudWatch metrics directly (i.e. avoid relying on CloudWatchLogs for exposing metrics about our ability to send logs to CloudWatchLogs).
* Output aggregate statistics about log delivery to STDOUT (i.e. to CloudWatchLogs). This should be useful in cases where the above alarms, but shouldn't be used as the trigger for alarms.


