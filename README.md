# logspout-cloudwatchlogs

Custom Logspout adaptor for sending logs to CloudWatchLogs

# API

Logspout handles taking a "route" that corresponds to this adapter and handles passing logs to it. This can be passed as an argument to the logstash process as follows:

    'cloudwatchlogs://AWSREGION?option1=value1&option2=value2'

For example, the following sends all container logs to a "all-the-logs" log group in the eu-west-1 region (maybe not the best idea):

    'cloudwatchlogs://eu-west-1?stream-prefix=i-d34db33f&default-log-group=other-logs'

## Scheme

The URL scheme should be set to "cloudwatchlogs" in order for the adapter to handle log messages.

## Options

### Global options

#### `stream-prefix` (required)

CloudWatch Streams (within a Log Group) are designed to accept a steam of logs from a single source. To enforce this calls to put logs return a sequence number that must be provided with a subsequent call to put logs. A missing or incorrect sequence number results in an error. This error does contain the correct sequence number so it is possible to recover from this error, but it is a bad idea to abuse this to effect interleaving logs from multiple sources: doing so would result in a race condition between multiple log sources competing to send log events, each causing a denial of service in the other(s).

From a single container host this adapter keeps the stream names unique by including the container name and source (i.e. "stdout" or "stderr"). However as it is possible that containers on separate hosts will have the same name, a unique prefix is applied - from the "stream-prefix" parameter. It is therefore important that this value is kept unique - good values for this would be the hostname or machine identifier for the container host (e.g. EC2 instance identifier). The format of the stream name is:

    "{stream-prefix}-{container-name}-{stream}"

For example, if you provide the `stream-prefix` "i-d34db33f" then a container called "complaining-harrold" would result in streams called "i-d34db33f-complaining-harrold-stdout" and "i-d34db33f-complaining-harrold-stderr".

### Log group selection

*At least on of `default-log-group`, `log-group-env-prefix` or `container-log-groups` is required in order for any logs to be sent to CloudWatchLogs.*

#### `default-log-group=LogGroupName`

For containers not matched by one of other mechanisms described here, logs will be sent to this log group.

#### `log-group-env-prefix=ENV_PREFIX`

This adapter supports discovering the CloudWatch Log Group from environment variables within the container. To enable this, set this variable - for example:

    'cloudwatchlogs://eu-west-1?log-group-env-prefix=LOGSPOUT_CLOUDWATCHLOGS'

This will cause the adapter to look for the "LOGSPOUT\_CLOUDWATCHLOGS\_STDOUT\_LOG\_GROUP" and "LOGSPOUT\_CLOUDWATCHLOGS\_STDERR\_LOG\_GROUP" environment variables on the container in order to determine where a container's logs should be sent.

#### `container-log-groups=container-name1:LogGroupName1,container-name2:LogGroupName2`

The maps specific container names to log groups. Each key (container name) and value (log group name) is separated by a colon (":"), and each pair is separated by a comma (","). For example, if you have containers called "ecs-agent" and "monitoring-agent", logs from these containers can be mapped to log groups with:

    'cloudwatchlogs://eu-west-1?container-log-groups=ecs-agent:ECSAgentLogGroup,monitoring-agent:MonitoringAgentLogGroup

