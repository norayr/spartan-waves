you can use this player, and build with

```
make
```

but you can also do

for mp3 stream

```
echo 'radio.norayr.am /radio 0' |nc norayr.am 300 |sox -tmp3 - -d
```

for ogg stream:
```
echo 'radio.norayr.am /radio 0' | nc norayr.am 300 | sox -V0 -togg  -  -d
```

and it'll play.
