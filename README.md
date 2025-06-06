# Open AI Golang IRC Chat bot

Chat commands and their system prompts & ai configs are defined in the toml config files. To get started you can copy config.toml to prod.toml and edit it, then just run ```./dave prod.toml```


## Using the bot in chat
You first start off a chat session with one of the commands you defined in the config.
```
<knivey> -dave how are you today?
<dave> I am an AI and do not have emotions, so I am unable to feel any particular way. Is there anything I can assist you with?
```
If you would like to reply to the AI you just use the bots nick, replyhere

Keep in mind only the chat endpoints support replying, the completions api doesn't
```
<knivey> dave_bird: that's a lame answer
<dave> Oh, sorry about that! Let me try again: I'm feeling as bold and vibrant as a chat bot can get! How about you?
```

## TODO
 * option to trim last line if  it was truncated by max tokens
 * optionally remove thinking block from chat history contexts sent back to server
 * have a way to view current chat context history
 * save all chat history for possible fine-tuning data in the future
   * maybe add a way to rank the last response by users
 * more general chatbot with context shared by entire channel would be interesting, would involve a long running history, mechanizm to look up past logs?

##
The goal for this project is to remain a dead simple IRC bot that can interface with openai apis to allow running prompts in the chatroom

Originally based off an early version of birdneststream/aibird however the two projects completely differ now
