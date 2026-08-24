# Mattermost channel compatibility

Tintwire provides a bounded compatibility surface for integrations that publish
through Mattermost incoming webhooks, bot API calls, or custom slash commands.
Compatibility intentionally covers the endpoints documented in the main
README; Tintwire is not a general Mattermost server replacement.

Incoming webhooks can preserve existing `/hooks/{id}` paths. An unlocked hook
may honor a payload channel override only when it names an existing public
Tintwire channel. Administrators can lock a hook to its configured channel.

Imported bots are mapped to one Tintwire user, team alias, and initial channel.
Additional channel grants are explicit. Imported slash commands retain their
trigger and response behavior while Tintwire continues to enforce its own
channel membership and action-target safety rules.
